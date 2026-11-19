"""ConferenceApp's device handling against an in-process fake server (control on TCP, the
media REGISTER on UDP): devices that cannot be opened, a camera that fails while it
runs, camera and microphone toggles, and leaving while things fail."""

from __future__ import annotations

import asyncio
import secrets
import threading
import time
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from typing import Any

import pytest

from confclient.app import VIDEO_FAILURE_LIMIT, ClientConfig, ConferenceApp, Devices
from confclient.devices import CapturedFrame, FakeCamera, FakeMicrophone, FakeSpeaker, VideoSource
from confclient.protocol import encode_frame, read_frame

from .test_media import FakeServer, fake_server


@dataclass
class FakeControl:
    """Answers like the real server for one client in room 1: `joined`, the roster
    copy that answers media_state (with its seq), `left`."""

    media_port: int
    requests: list[dict[str, Any]] = field(default_factory=list)
    video: bool = True
    audio: bool = True

    def roster(self, seq: int | None = None) -> dict[str, Any]:
        me = {
            "participant_id": 1,
            "name": "alice",
            "audio_ssrc": 11,
            "video_ssrc": 12,
            "video": self.video,
            "audio": self.audio,
            "registered": True,
        }
        msg: dict[str, Any] = {"type": "roster", "room_id": 1, "host_id": 1, "participants": [me]}
        if seq is not None:
            msg["seq"] = seq
        return msg

    async def serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            while True:
                req = await read_frame(reader)
                self.requests.append(req)
                kind, seq = req["type"], req.get("seq")
                if kind == "hello":
                    reply: dict[str, Any] = {
                        "type": "welcome",
                        "client_id": 1,
                        "version": 1,
                        "server_time_ms": 0,
                        "media_port": self.media_port,
                    }
                elif kind in ("create_room", "join_room"):
                    reply = {
                        "type": "joined",
                        "room_id": 1,
                        "title": "t",
                        "host": True,
                        "participant_id": 1,
                        "audio_ssrc": 11,
                        "video_ssrc": 12,
                        "mixer_ssrc": 13,
                        "token": secrets.token_hex(16),
                        "media_port": self.media_port,
                        "max_participants": 16,
                    }
                elif kind == "media_state":
                    self.video, self.audio = req["video"], req["audio"]
                    reply = self.roster()
                elif kind == "leave_room":
                    reply = {"type": "left", "room_id": 1}
                else:
                    reply = {"type": "error", "code": "bad_request", "message": kind}
                writer.write(encode_frame({**reply, "seq": seq}))
                await writer.drain()
        except (asyncio.IncompleteReadError, ConnectionError):
            pass
        finally:
            writer.close()

    def media_states(self) -> list[tuple[bool, bool]]:
        return [(r["video"], r["audio"]) for r in self.requests if r["type"] == "media_state"]


@asynccontextmanager
async def fake_servers() -> AsyncIterator[tuple[FakeControl, FakeServer, int]]:
    async with fake_server() as media:
        control = FakeControl(media.port)
        server = await asyncio.start_server(control.serve, "127.0.0.1", 0)
        try:
            yield control, media, int(server.sockets[0].getsockname()[1])
        finally:
            server.close()
            await server.wait_closed()


def _run(test: Callable[[], Any]) -> None:
    asyncio.run(asyncio.wait_for(test(), 20))


def _camera() -> FakeCamera:
    return FakeCamera(160, 120, fps=50, loop_frames=2)


def _app(port: int, devices: Devices, log: list[str], **cfg: Any) -> ConferenceApp:
    return ConferenceApp(ClientConfig(name="alice", port=port, **cfg), devices, log.append)


def _no_camera() -> FakeCamera:
    raise RuntimeError("camera 0 cannot be opened")


def _no_microphone() -> FakeMicrophone:
    raise OSError(-9996, "Invalid input device (no default output device)")


async def _within(seconds: float, aw: Any) -> Any:
    """Await aw, failing if it takes longer than `seconds` (the old media_state wait was
    a 5 s timeout)."""
    start = time.monotonic()
    result = await asyncio.wait_for(aw, seconds)
    assert time.monotonic() - start < seconds
    return result


def test_a_missing_camera_and_microphone_do_not_stop_a_join() -> None:
    """Review finding client-2: a device that could not be opened crashed create/join."""

    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            log: list[str] = []
            app = _app(port, Devices(_no_camera, _no_microphone, FakeSpeaker), log)
            await app.start()
            await _within(3, app.create("t"))
            assert app.room is not None
            assert app.session is not None
            assert (app.video_on, app.audio_on) == (False, False)
            assert "! camera unavailable: camera 0 cannot be opened" in log
            assert any(line.startswith("! microphone unavailable: ") for line in log)
            assert "* joined with video off, audio off" in log
            assert control.media_states() == [(False, False)]  # the others see both off
            # Turning the camera on again tries again, and fails the same way.
            assert await _within(2, app.set_video(True)) is False
            assert log.count("! camera unavailable: camera 0 cannot be opened") == 2
            await _within(2, app.leave())
            await app.close()

    _run(scenario)


def test_devices_that_are_off_are_not_opened() -> None:
    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            opened: list[str] = []

            def camera() -> FakeCamera:
                opened.append("camera")
                return _camera()

            def mic() -> FakeMicrophone:
                opened.append("microphone")
                return FakeMicrophone()

            log: list[str] = []
            app = _app(port, Devices(camera, mic, FakeSpeaker), log, video_on=False, audio_on=False)
            await app.start()
            await _within(3, app.create("t"))  # with video_on=False: no 5 s wait (protocol-1)
            assert opened == []
            assert control.media_states() == [(False, False)]
            assert not any(line.startswith("!") for line in log), log
            assert await _within(2, app.set_audio(True)) is True
            assert await _within(2, app.set_video(True)) is True
            assert opened == ["microphone", "camera"]
            assert control.media_states()[-1] == (True, True)
            assert await _within(2, app.set_video(False)) is False
            assert await _within(2, app.set_audio(False)) is False
            assert control.media_states()[-1] == (False, False)
            await app.leave()
            await app.close()

    _run(scenario)


def test_toggles_answer_quickly_and_reopen_the_devices() -> None:
    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            cams: list[FakeCamera] = []

            def camera() -> FakeCamera:
                cams.append(_camera())
                return cams[-1]

            log: list[str] = []
            app = _app(port, Devices(camera, FakeMicrophone, FakeSpeaker), log)
            await app.start()
            await _within(3, app.create("t"))
            assert control.media_states() == []  # both on: nothing to announce
            for on in (False, True, False, True):
                assert await _within(2, app.set_video(on)) is on
                assert await _within(2, app.set_audio(on)) is on
            assert len(cams) == 3  # opened at the join and at each `video on`
            assert control.media_states()[-1] == (True, True)
            await app.leave()
            await app.close()
            assert not any(line.startswith("!") for line in log), log

    _run(scenario)


class FlakyCamera(VideoSource):
    """Delivers `good` frames, then raises on every capture."""

    fps = 100.0

    def __init__(self, good: int) -> None:
        self.inner = _camera()
        self.good = good
        self.captures = 0
        self.closed = threading.Event()

    def capture(self) -> CapturedFrame | None:
        self.captures += 1
        if self.captures > self.good:
            raise OSError("device unplugged")
        return self.inner.capture()

    def close(self) -> None:
        self.closed.set()


def test_a_failing_camera_is_reported_once_and_turned_off() -> None:
    """Review finding client-3: one exception stopped the capture loop silently, and
    leave()/close() then raised it."""

    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            cam = FlakyCamera(good=3)
            log: list[str] = []
            app = _app(port, Devices(lambda: cam, FakeMicrophone, FakeSpeaker), log)
            await app.start()
            await app.create("t")
            for _ in range(300):
                if not app.video_on and control.media_states():
                    break
                await asyncio.sleep(0.01)
            assert not app.video_on
            assert control.media_states() == [(False, True)]
            assert log.count("! camera error: OSError('device unplugged')") == 1
            assert f"! camera turned off after {VIDEO_FAILURE_LIMIT} errors in a row" in log
            assert cam.captures == 3 + VIDEO_FAILURE_LIMIT
            assert cam.closed.is_set()
            await _within(2, app.leave())
            await _within(2, app.close())

    _run(scenario)


class SlowCamera(VideoSource):
    """A capture that blocks until released; records whether close() ran during it."""

    fps = 10.0

    def __init__(self) -> None:
        self.in_capture = threading.Event()
        self.release = threading.Event()
        self.closed_during_capture = False
        self._busy = False

    def capture(self) -> CapturedFrame | None:
        self._busy = True
        self.in_capture.set()
        self.release.wait(5)
        self._busy = False
        return None

    def close(self) -> None:
        self.closed_during_capture = self._busy


def test_the_camera_is_not_closed_while_a_capture_is_running() -> None:
    """Review finding client-9: camera.close() ran while the capture thread was still in
    the device."""

    async def scenario() -> None:
        async with fake_servers() as (_control, _media, port):
            cam = SlowCamera()
            app = _app(port, Devices(lambda: cam, FakeMicrophone, FakeSpeaker), [])
            await app.start()
            await app.create("t")
            await asyncio.to_thread(cam.in_capture.wait, 5)
            off = asyncio.create_task(app.set_video(False))
            await asyncio.sleep(0.2)
            assert not off.done()  # waits for the capture to come back
            cam.release.set()
            assert await asyncio.wait_for(off, 5) is False
            assert not cam.closed_during_capture
            await app.leave()
            await app.close()

    _run(scenario)


def test_close_right_after_a_cancelled_command_returns() -> None:
    """Review finding client-3: close() swallowed the caller's own cancellation, so
    `cancel` followed by an immediate exit could hang."""

    async def scenario() -> None:
        async with fake_servers() as (_control, _media, port):
            app = _app(port, Devices(_camera, FakeMicrophone, FakeSpeaker), [])
            await app.start()
            await app.create("t")
            closing = asyncio.create_task(app.close())
            await asyncio.sleep(0)
            closing.cancel()
            with pytest.raises(asyncio.CancelledError):
                await closing
            await _within(3, app.close())  # a second close still finishes the job
            assert app.session is None
            assert app.room is None

    _run(scenario)


class BrokenSpeaker(FakeSpeaker):
    def start(self, pull: Callable[[], Any]) -> None:
        raise OSError("no output device")


def test_a_speaker_that_fails_to_start_leaves_the_rest_working() -> None:
    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            log: list[str] = []
            app = _app(port, Devices(_camera, FakeMicrophone, BrokenSpeaker), log)
            await app.start()
            await app.create("t")
            assert "! speaker unavailable: no output device" in log
            assert (app.video_on, app.audio_on) == (True, True)
            assert control.media_states() == []
            await app.leave()
            await app.close()

    _run(scenario)


def test_no_device_support_at_all() -> None:
    """Without OpenCV and PyAudio the factories are None: the client still joins."""

    async def scenario() -> None:
        async with fake_servers() as (control, _media, port):
            log: list[str] = []
            app = _app(port, Devices(None, None, None), log)
            await app.start()
            await app.create("t")
            assert (app.video_on, app.audio_on) == (False, False)
            assert "! camera unavailable: no camera support installed" in log
            assert control.media_states() == [(False, False)]
            await app.leave()
            await app.close()

    _run(scenario)
