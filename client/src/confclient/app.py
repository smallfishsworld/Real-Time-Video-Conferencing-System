"""The conference client: control connection, room membership, media session, devices
and (optionally) the split-screen window."""

from __future__ import annotations

import asyncio
import contextlib
import datetime as dt
from collections.abc import Callable, Coroutine
from dataclasses import dataclass, field
from typing import Any, TypeVar

from .aio import cancel_and_wait
from .assembler import VideoFrame
from .control import ControlClient, ServerError
from .devices import AudioSink, AudioSource, VideoSource
from .jitter import AudioPlayout
from .media import MediaConfig, MediaSession
from .protocol import ProtocolError
from .testpattern import Image8, decode_jpeg
from .ui import GridRenderer, OpenCVWindow, Tile

Output = Callable[[str], None]
T = TypeVar("T")
# What a tile shows: name, participant id (None for this client), latest frame, status.
TileInput = tuple[str, int | None, VideoFrame | None, str]

# Capture errors in a row after which the camera is turned off.
VIDEO_FAILURE_LIMIT = 5


@dataclass
class ClientConfig:
    name: str
    server: str = "127.0.0.1"
    port: int = 6100
    media: MediaConfig = field(default_factory=MediaConfig)
    video_on: bool = True
    audio_on: bool = True


@dataclass(slots=True)
class ChatLine:
    room_id: int
    from_id: int
    from_name: str
    text: str
    server_time_ms: int

    def format(self) -> str:
        stamp = dt.datetime.fromtimestamp(self.server_time_ms / 1000).strftime("%Y-%m-%d %H:%M:%S")
        return f"[{stamp}] {self.from_name}: {self.text}"


@dataclass
class Devices:
    """Factories for the media devices. A device is opened only while it is needed: the
    speaker for each room membership, the camera and the microphone while they are on.
    A factory may raise (no such device, no permission); the client then carries on
    with that device off."""

    video: Callable[[], VideoSource] | None
    microphone: Callable[[], AudioSource] | None
    speaker: Callable[[], AudioSink] | None


class ConferenceApp:
    def __init__(
        self, cfg: ClientConfig, devices: Devices, out: Output = print, window: bool = False
    ) -> None:
        self.cfg = cfg
        self.devices = devices
        self.out = out
        self.control = ControlClient(cfg.server, cfg.port)
        self.room: dict[str, Any] | None = None
        self.roster: dict[str, Any] | None = None
        self.mode: dict[str, Any] | None = None
        self.session: MediaSession | None = None
        self.playout = AudioPlayout(cfg.media.audio_delay_frames)
        self.chat_log: list[ChatLine] = []
        self.remote_frames: dict[int, VideoFrame] = {}
        self.local_preview: Image8 | None = None
        self.video_on = cfg.video_on
        self.audio_on = cfg.audio_on
        self.room_closed = asyncio.Event()
        self._window_wanted = window
        self._window: OpenCVWindow | None = None
        self._renderer = GridRenderer()
        self._decoded: dict[int, tuple[int, Image8]] = {}  # used by the render thread only
        self._camera: VideoSource | None = None
        self._camera_task: asyncio.Task[None] | None = None
        self._capture: asyncio.Future[Any] | None = None  # the capture running in a thread
        self._mic: AudioSource | None = None
        self._speaker: AudioSink | None = None
        self._tasks: list[asyncio.Task[None]] = []
        self._background: set[asyncio.Task[Any]] = set()

    # Session --------------------------------------------------------------------

    async def start(self) -> dict[str, Any]:
        """Connect and say hello. Raises ServerError if the server refuses the hello
        and OSError or TimeoutError if it cannot be reached."""
        try:
            welcome = await self.control.connect(self.cfg.name)
        except BaseException:
            await self.control.close()
            raise
        self._tasks.append(asyncio.create_task(self._event_pump(), name="event-pump"))
        if self._window_wanted:
            self._tasks.append(asyncio.create_task(self._render_loop(), name="render"))
        return welcome

    async def close(self) -> None:
        try:
            await self._exit_room()
        finally:
            tasks, self._tasks = self._tasks, []
            await cancel_and_wait([*tasks, *self._background], self.out)
            self._background.clear()
            await self.control.close()
            if self._window is not None:
                self._window.close()
                self._window = None

    def _spawn(self, coro: Coroutine[Any, Any, Any], name: str) -> None:
        task = asyncio.create_task(coro, name=name)
        self._background.add(task)
        task.add_done_callback(self._background.discard)

    # Commands -------------------------------------------------------------------

    async def list_rooms(self) -> list[dict[str, Any]]:
        reply = await self.control.request({"type": "list_rooms"})
        rooms: list[dict[str, Any]] = reply.get("rooms", [])
        return rooms

    async def create(self, title: str = "") -> dict[str, Any]:
        joined = await self.control.request({"type": "create_room", "title": title})
        await self._enter(joined)
        return joined

    async def join(self, room_id: int) -> dict[str, Any]:
        joined = await self.control.request({"type": "join_room", "room_id": room_id})
        await self._enter(joined)
        return joined

    async def leave(self) -> None:
        await self.control.request({"type": "leave_room"})
        await self._exit_room()

    async def cancel(self) -> None:
        await self.control.request({"type": "cancel_room"})
        await self._exit_room()

    async def chat(self, text: str) -> None:
        """Send a chat message. The server's reply is this client's own copy of the
        message (shown like everyone else's); a refusal raises ServerError."""
        await self.control.request({"type": "chat", "text": text})

    async def set_video(self, on: bool) -> bool:
        """Turn the camera on or off; returns whether it is on. In a meeting the camera
        is opened here, and if it cannot be opened video stays off."""
        if on and self.room is not None and self._camera is None:
            on = await self._start_camera()
        elif not on:
            await self._stop_camera()
        self.video_on = on
        await self._send_media_state()
        return on

    async def set_audio(self, on: bool) -> bool:
        """Turn the microphone on or off; returns whether it is on."""
        if on and self.room is not None and self._mic is None:
            on = await self._start_mic()
        elif not on:
            self._stop_mic()
        self.audio_on = on
        await self._send_media_state()
        return on

    async def _send_media_state(self) -> None:
        if self.room is not None:
            # The reply is this client's copy of the updated roster.
            await self.control.request(
                {"type": "media_state", "video": self.video_on, "audio": self.audio_on}
            )

    # Devices ----------------------------------------------------------------------

    async def _open_device(self, what: str, factory: Callable[[], T] | None) -> T | None:
        """Run a device factory in a worker thread (opening a device can block); None
        and a message if there is no such device or it cannot be opened."""
        if factory is None:
            self.out(f"! {what} unavailable: no {what} support installed")
            return None
        try:
            return await asyncio.to_thread(factory)
        except Exception as exc:  # any driver or library error: carry on without it
            self.out(f"! {what} unavailable: {exc}")
            return None

    async def _start_camera(self) -> bool:
        camera = await self._open_device("camera", self.devices.video)
        if camera is None:
            return False
        if self.room is None or self._camera is not None:  # left, or opened meanwhile
            await self._close_camera(camera)
            return self._camera is not None
        self._camera = camera
        self._camera_task = asyncio.create_task(self._video_loop(camera), name="video-capture")
        return True

    async def _stop_camera(self) -> None:
        camera, self._camera = self._camera, None
        task, self._camera_task = self._camera_task, None
        if task is not None:
            await cancel_and_wait([task], self.out)
        capture, self._capture = self._capture, None
        if capture is not None:
            # The capture keeps running in its thread after the task was cancelled; the
            # device must not be closed under it.
            await asyncio.wait([capture])
            if not capture.cancelled():
                capture.exception()  # retrieved: a failure here no longer matters
        if camera is not None:
            await self._close_camera(camera)
        self.local_preview = None

    async def _close_camera(self, camera: VideoSource) -> None:
        try:
            await asyncio.to_thread(camera.close)
        except Exception as exc:
            self.out(f"! camera close failed: {exc}")

    async def _start_mic(self) -> bool:
        mic = await self._open_device("microphone", self.devices.microphone)
        if mic is None:
            return False
        if self.room is None or self._mic is not None:
            mic.stop()
            return self._mic is not None
        try:
            mic.start(asyncio.get_running_loop(), self._on_mic_frame)
        except Exception as exc:
            self.out(f"! microphone unavailable: {exc}")
            self._stop_device("microphone", mic.stop)
            return False
        self._mic = mic
        return True

    def _stop_mic(self) -> None:
        mic, self._mic = self._mic, None
        if mic is not None:
            self._stop_device("microphone", mic.stop)

    def _stop_device(self, what: str, stop: Callable[[], None]) -> None:
        try:
            stop()
        except Exception as exc:
            self.out(f"! {what} stop failed: {exc}")

    async def _start_speaker(self) -> None:
        speaker = await self._open_device("speaker", self.devices.speaker)
        if speaker is None:
            return
        try:
            speaker.start(self.playout.pull)
        except Exception as exc:
            self.out(f"! speaker unavailable: {exc}")
            self._stop_device("speaker", speaker.stop)
            return
        self._speaker = speaker

    # Room membership --------------------------------------------------------------

    async def _enter(self, joined: dict[str, Any]) -> None:
        self.room = joined
        self.room_closed.clear()
        self.remote_frames.clear()
        session: MediaSession | None = None
        try:
            # The media path goes to the address the control connection reached, so a
            # name that resolves to several addresses cannot split the two.
            server = self.control.peer_host or self.cfg.server
            session = MediaSession(server, joined, self.playout, self._on_video, config=self.cfg.media)
            await session.start()
        except Exception:
            # Without a media path the membership is useless: give the seat back.
            if session is not None:
                await session.close()
            self.room = None
            with contextlib.suppress(ServerError, ConnectionError, TimeoutError):
                await self.control.request({"type": "leave_room"})
            raise
        self.session = session
        if self.roster is not None and self.roster.get("room_id") == joined["room_id"]:
            session.update_roster(self.roster)
        if self.mode is not None and self.mode.get("room_id") == joined["room_id"]:
            session.update_mode(self.mode)
        if self.devices.speaker is not None:
            await self._start_speaker()
        wanted = (self.video_on, self.audio_on)
        if self.audio_on:
            self.audio_on = await self._start_mic()
        if self.video_on:
            self.video_on = await self._start_camera()
        if not (self.video_on and self.audio_on):
            if (self.video_on, self.audio_on) != wanted:
                video, audio = self._onoff(self.video_on), self._onoff(self.audio_on)
                self.out(f"* joined with video {video}, audio {audio}")
            await self._send_media_state()

    @staticmethod
    def _onoff(on: bool) -> str:
        return "on" if on else "off"

    async def _exit_room(self) -> None:
        """Stop the devices and the media session. Safe to run twice at once (the event
        pump and a command can both end the membership): each step takes what it closes."""
        self.room = None
        try:
            await self._stop_camera()
            self._stop_mic()
            speaker, self._speaker = self._speaker, None
            if speaker is not None:
                self._stop_device("speaker", speaker.stop)
        finally:
            session, self.session = self.session, None
            if session is not None:
                await session.close()
            self.roster = None
            self.mode = None

    async def _video_loop(self, camera: VideoSource) -> None:
        failures = 0
        warned_size = False
        while True:
            self._capture = capture = asyncio.ensure_future(asyncio.to_thread(camera.capture))
            try:
                frame = await asyncio.shield(capture)
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                failures += 1
                if failures == 1:
                    self.out(f"! camera error: {exc!r}")
                if failures >= VIDEO_FAILURE_LIMIT:
                    self.out(f"! camera turned off after {failures} errors in a row")
                    self._spawn(self.set_video(False), "video-off")
                    return
                await asyncio.sleep(1 / max(float(getattr(camera, "fps", 10.0)), 1.0))
                continue
            self._capture = None
            failures = 0
            if frame is None or not self.video_on:
                continue
            self.local_preview = frame.image
            if self.session is not None:
                try:
                    self.session.send_video(frame.jpeg, frame.captured)
                except ProtocolError as exc:  # a frame too large for the wire format
                    if not warned_size:
                        self.out(f"! video frame not sent: {exc}")
                        warned_size = True

    def _on_mic_frame(self, pcm_be: bytes, captured: float) -> None:
        if self.audio_on and self.session is not None:
            self.session.send_audio(pcm_be, captured)

    def _on_video(self, participant_id: int, frame: VideoFrame) -> None:
        self.remote_frames[participant_id] = frame

    # Events -----------------------------------------------------------------------

    def _names(self) -> dict[int, str]:
        if self.roster is None:
            return {}
        return {int(p["participant_id"]): str(p["name"]) for p in self.roster.get("participants", [])}

    def _current_room(self, ev: dict[str, Any]) -> bool:
        """True unless the event belongs to a room this client is no longer in. While a
        join is in flight (no room yet) events are kept, and `_enter` applies the latest
        roster and mode once the media session exists."""
        return self.room is None or ev.get("room_id") == self.room.get("room_id")

    async def _event_pump(self) -> None:
        while True:
            ev = await self.control.events.get()
            try:
                if not await self._on_event(ev):
                    return
            except (KeyError, TypeError, ValueError) as exc:  # a malformed event must not stop the pump
                self.out(f"! ignored malformed {ev.get('type')!r} event: {exc!r}")

    async def _on_event(self, ev: dict[str, Any]) -> bool:
        kind = ev.get("type")
        if kind == "roster":
            if self._current_room(ev):
                self._on_roster(ev)
        elif kind == "mode":
            if not self._current_room(ev):
                return True
            self.mode = ev
            if self.session is not None:
                self.session.update_mode(ev)
            if ev.get("mode") == "p2p":
                peer = self._names().get(int(ev["peer"]["participant_id"]), "peer")
                self.out(f"* media mode: peer-to-peer with {peer} ({ev['peer']['addr']})")
            else:
                self.out("* media mode: server relay")
        elif kind == "chat":
            line = ChatLine(
                int(ev["room_id"]),
                int(ev["from_id"]),
                str(ev["from_name"]),
                str(ev["text"]),
                int(ev["server_time_ms"]),
            )
            self.chat_log.append(line)
            self.out(line.format())
        elif kind == "error":
            # An error that answers no pending request, such as a frame refused by the
            # server's request rate limit before it was read.
            self.out(f"! server: {ev.get('code')}: {ev.get('message')}")
        elif kind == "room_closed":
            if self.room is not None and ev.get("room_id") == self.room.get("room_id"):
                self.out(f"* meeting {ev.get('room_id')} closed ({ev.get('reason')})")
                await self._exit_room()
                self.room_closed.set()
        elif kind == "disconnected":
            self.out("* disconnected from server")
            await self._exit_room()
            self.room_closed.set()
            return False
        return True

    def _on_roster(self, roster: dict[str, Any]) -> None:
        before = self._names()
        self.roster = roster
        after = self._names()
        if self.session is not None:
            self.session.update_roster(roster)
        for pid, name in after.items():
            if pid not in before and before:
                self.out(f"* {name} joined")
        for pid, name in before.items():
            if pid not in after:
                self.out(f"* {name} left")
                self.remote_frames.pop(pid, None)

    # Window -----------------------------------------------------------------------

    def _tile_snapshot(self) -> list[TileInput]:
        """What each tile shows, taken on the event loop: (name, participant id or None
        for this client, the latest frame, status text)."""
        out: list[TileInput] = [(f"{self.cfg.name} (you)", None, None, "" if self.video_on else "camera off")]
        if self.roster is None:
            return out
        me = (self.room or {}).get("participant_id")
        for p in self.roster.get("participants", []):
            pid = int(p["participant_id"])
            if pid == me:
                continue
            video = bool(p.get("video", True))
            frame = self.remote_frames.get(pid) if video else None
            out.append((str(p["name"]), pid, frame, "waiting for video" if video else "camera off"))
        return out

    def _tiles_from(self, snapshot: list[TileInput], preview: Image8 | None) -> list[Tile]:
        """Decode new frames (in the render thread) and build the tiles."""
        tiles: list[Tile] = []
        shown: set[int] = set()
        for name, pid, frame, status in snapshot:
            if pid is None:
                tiles.append(Tile(name, preview, status))
                continue
            shown.add(pid)
            image = None
            if frame is not None:
                cached = self._decoded.get(pid)
                if cached is None or cached[0] != frame.frame_id:
                    try:
                        fit = (self._renderer.tile_width, self._renderer.tile_height)
                        cached = (frame.frame_id, decode_jpeg(frame.data, fit))
                        self._decoded[pid] = cached
                    except (OSError, ValueError):
                        cached = None
                image = cached[1] if cached else None
            tiles.append(Tile(name, image, status))
        for pid in [p for p in self._decoded if p not in shown]:
            del self._decoded[pid]
        return tiles

    def tiles(self) -> list[Tile]:
        """The tiles the window would show now, for headless use: it decodes on the
        calling thread, so it must not run while the window's render loop does."""
        return self._tiles_from(self._tile_snapshot(), self.local_preview)

    async def _render_loop(self) -> None:
        self._window = OpenCVWindow(f"conference - {self.cfg.name}")
        while True:
            await asyncio.sleep(0.05)
            snapshot = self._tile_snapshot()
            preview = self.local_preview
            # JPEG decoding and compositing run in a worker thread; only the window
            # update stays on this (the main) thread, as OpenCV requires on macOS.
            canvas = await asyncio.to_thread(self._compose, snapshot, preview)
            self._window.show(canvas)

    def _compose(self, snapshot: list[TileInput], preview: Image8 | None) -> Image8:
        return self._renderer.compose(self._tiles_from(snapshot, preview))


__all__ = ["ChatLine", "ClientConfig", "ConferenceApp", "Devices", "ServerError"]
