"""The conference client: control connection, room membership, media session, devices
and (optionally) the split-screen window."""

from __future__ import annotations

import asyncio
import contextlib
import datetime as dt
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from .assembler import VideoFrame
from .control import ControlClient, ServerError
from .devices import AudioSink, AudioSource, VideoSource
from .jitter import AudioPlayout
from .media import MediaConfig, MediaSession
from .testpattern import Image8, decode_jpeg
from .ui import GridRenderer, OpenCVWindow, Tile

Output = Callable[[str], None]


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
    """Factories for the media devices; each room membership opens fresh devices."""

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
        self._decoded: dict[int, tuple[int, Image8]] = {}
        self._camera: VideoSource | None = None
        self._mic: AudioSource | None = None
        self._speaker: AudioSink | None = None
        self._room_tasks: list[asyncio.Task[None]] = []
        self._tasks: list[asyncio.Task[None]] = []
        self._video_enabled = asyncio.Event()
        if self.video_on:
            self._video_enabled.set()

    # Session --------------------------------------------------------------------

    async def start(self) -> dict[str, Any]:
        welcome = await self.control.connect(self.cfg.name)
        self._tasks.append(asyncio.create_task(self._event_pump(), name="event-pump"))
        if self._window_wanted:
            self._tasks.append(asyncio.create_task(self._render_loop(), name="render"))
        return welcome

    async def close(self) -> None:
        await self._exit_room()
        for task in self._tasks:
            task.cancel()
        for task in self._tasks:
            with contextlib.suppress(asyncio.CancelledError):
                await task
        self._tasks.clear()
        await self.control.close()
        if self._window is not None:
            self._window.close()

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
        await self.control.send({"type": "chat", "text": text})

    async def set_video(self, on: bool) -> None:
        self.video_on = on
        if on:
            self._video_enabled.set()
        else:
            self._video_enabled.clear()
            self.local_preview = None
        await self._send_media_state()

    async def set_audio(self, on: bool) -> None:
        self.audio_on = on
        await self._send_media_state()

    async def _send_media_state(self) -> None:
        if self.room is not None:
            await self.control.request(
                {"type": "media_state", "video": self.video_on, "audio": self.audio_on}
            )

    # Room membership --------------------------------------------------------------

    async def _enter(self, joined: dict[str, Any]) -> None:
        self.room = joined
        self.room_closed.clear()
        self.remote_frames.clear()
        session = MediaSession(self.cfg.server, joined, self.playout, self._on_video, config=self.cfg.media)
        await session.start()
        self.session = session
        if self.roster is not None and self.roster.get("room_id") == joined["room_id"]:
            session.update_roster(self.roster)
        if self.mode is not None and self.mode.get("room_id") == joined["room_id"]:
            session.update_mode(self.mode)
        if self.devices.speaker is not None:
            self._speaker = self.devices.speaker()
            self._speaker.start(self.playout.pull)
        if self.devices.microphone is not None:
            self._mic = self.devices.microphone()
            self._mic.start(asyncio.get_running_loop(), self._on_mic_frame)
        if self.devices.video is not None:
            self._camera = self.devices.video()
            self._room_tasks.append(asyncio.create_task(self._video_loop(self._camera), name="video-capture"))
        if self.video_on is False or self.audio_on is False:
            await self._send_media_state()

    async def _exit_room(self) -> None:
        for task in self._room_tasks:
            task.cancel()
        for task in self._room_tasks:
            with contextlib.suppress(asyncio.CancelledError):
                await task
        self._room_tasks.clear()
        if self._mic is not None:
            self._mic.stop()
            self._mic = None
        if self._speaker is not None:
            self._speaker.stop()
            self._speaker = None
        if self._camera is not None:
            self._camera.close()
            self._camera = None
        if self.session is not None:
            await self.session.close()
            self.session = None
        self.room = None
        self.roster = None
        self.mode = None

    async def _video_loop(self, camera: VideoSource) -> None:
        while True:
            await self._video_enabled.wait()
            frame = await asyncio.to_thread(camera.capture)
            if frame is None or not self.video_on:
                continue
            self.local_preview = frame.image
            if self.session is not None:
                self.session.send_video(frame.jpeg, frame.captured)

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
                self._decoded.pop(pid, None)

    # Window -----------------------------------------------------------------------

    def tiles(self) -> list[Tile]:
        tiles = [Tile(f"{self.cfg.name} (you)", self.local_preview, "" if self.video_on else "camera off")]
        if self.roster is None:
            return tiles
        for p in self.roster.get("participants", []):
            pid = int(p["participant_id"])
            if pid == (self.room or {}).get("participant_id"):
                continue
            image = None
            frame = self.remote_frames.get(pid)
            if frame is not None and p.get("video", True):
                cached = self._decoded.get(pid)
                if cached is None or cached[0] != frame.frame_id:
                    with contextlib.suppress(OSError, ValueError):
                        cached = (frame.frame_id, decode_jpeg(frame.data))
                        self._decoded[pid] = cached
                image = cached[1] if cached else None
            status = "camera off" if not p.get("video", True) else "waiting for video"
            tiles.append(Tile(str(p["name"]), image, status))
        return tiles

    async def _render_loop(self) -> None:
        self._window = OpenCVWindow(f"conference - {self.cfg.name}")
        while True:
            await asyncio.sleep(0.05)
            self._window.show(self._renderer.compose(self.tiles()))


__all__ = ["ChatLine", "ClientConfig", "ConferenceApp", "Devices", "ServerError"]
