"""Command-line client: `python -m confclient --server 127.0.0.1 --name alice`."""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import importlib.util
import sys
import threading
import zlib
from collections.abc import Callable

from .app import ClientConfig, ConferenceApp, Devices
from .control import ServerError
from .devices import (
    AudioSink,
    AudioSource,
    FakeCamera,
    FakeMicrophone,
    FakeSpeaker,
    OpenCVCamera,
    PyAudioMicrophone,
    PyAudioSpeaker,
    VideoSource,
)
from .media import MediaConfig

HELP = """commands:
  list                  show open meetings
  create [title]        create a meeting and join it as host
  join <id>             join a meeting
  leave                 leave the current meeting (alias: quit)
  cancel                end the meeting for everyone (host only)
  chat <text>           send a chat message (also: say <text>, send:<text>)
  video on|off          turn the camera on or off (also: switch video)
  audio on|off          turn the microphone on or off (also: switch audio)
  status                show the meeting, mode and media counters
  sleep <seconds>       pause (useful in --script)
  help                  show this help
  exit                  leave and quit"""


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="confclient", description="Real-time video conferencing client")
    p.add_argument("--server", default="127.0.0.1", help="server host (default: 127.0.0.1)")
    p.add_argument("--port", type=int, default=6100, help="server control port (default: 6100)")
    p.add_argument("--name", required=True, help="display name")
    p.add_argument("--fake-media", action="store_true", help="synthetic camera and tone instead of devices")
    p.add_argument("--headless", action="store_true", help="no window")
    p.add_argument("--no-video", action="store_true", help="start with the camera off")
    p.add_argument("--no-audio", action="store_true", help="start with the microphone off")
    p.add_argument("--width", type=int, default=480)
    p.add_argument("--height", type=int, default=480)
    p.add_argument("--fps", type=float, default=15.0)
    p.add_argument("--jpeg-quality", type=int, default=85)
    p.add_argument(
        "--max-send-kbps", type=int, default=6000, help="cap on this client's upload (default: 6000)"
    )
    p.add_argument("--video-fec-k", type=int, default=4, help="video parity group size, 0 disables FEC")
    p.add_argument("--audio-fec-k", type=int, default=2, help="audio parity group size, 0 disables FEC")
    p.add_argument("--jitter-ms", type=int, default=60, help="audio playout delay (multiple of 20 ms)")
    p.add_argument("--create", nargs="?", const="", metavar="TITLE", help="create a meeting at start")
    p.add_argument("--join", type=int, metavar="ID", help="join a meeting at start")
    p.add_argument(
        "--script",
        metavar="COMMANDS",
        help='commands to run at start, separated by ";" (for example "create demo; sleep 10; exit")',
    )
    p.add_argument("--duration", type=float, help="leave and exit after this many seconds")
    return p


def make_devices(args: argparse.Namespace) -> Devices:
    if args.fake_media:
        freq = 300 + zlib.crc32(args.name.encode()) % 500

        def camera() -> VideoSource:
            return FakeCamera(args.width, args.height, args.fps, args.jpeg_quality)

        def mic() -> AudioSource:
            return FakeMicrophone(freq=freq)

        def speaker() -> AudioSink:
            return FakeSpeaker()

        return Devices(camera, mic, speaker)

    def real_camera() -> VideoSource:
        return OpenCVCamera(0, args.width, args.height, args.fps, args.jpeg_quality)

    have_cv2 = importlib.util.find_spec("cv2") is not None
    have_pa = importlib.util.find_spec("pyaudio") is not None
    if not have_cv2:
        print("opencv-python is not installed: camera disabled (pip install 'confclient[video]')")
    if not have_pa:
        print("pyaudio is not installed: audio disabled (pip install 'confclient[audio]')")
    return Devices(
        real_camera if have_cv2 else None,
        PyAudioMicrophone if have_pa else None,
        PyAudioSpeaker if have_pa else None,
    )


def config_from_args(args: argparse.Namespace) -> ClientConfig:
    media = MediaConfig(
        max_send_kbps=args.max_send_kbps,
        video_fec_k=args.video_fec_k,
        audio_fec_k=args.audio_fec_k,
        audio_delay_frames=max(1, args.jitter_ms // 20),
    )
    return ClientConfig(
        name=args.name,
        server=args.server,
        port=args.port,
        media=media,
        video_on=not args.no_video,
        audio_on=not args.no_audio,
    )


def _status(app: ConferenceApp) -> str:
    if app.room is None:
        return "not in a meeting"
    lines = [f"meeting {app.room['room_id']} ({app.room['title']}), host: {app.room['host']}"]
    s = app.session
    if s is not None:
        lines.append(f"mode: {s.mode}, sending to {s.targets}")
        lines.append(
            f"sent: {s.stats.video_frames_sent} video frames, {s.stats.audio_frames_sent} audio frames"
            f" ({s.stats.video_frames_rate_limited} frames held back by the bitrate cap)"
        )
        lines.append(f"received frames by participant: {s.stats.frames_by_participant}")
        lines.append(f"FEC recovered packets: {s.stats.fec_recovered}")
    lines.append(
        f"audio: {app.playout.frames_played} frames played, {app.playout.frames_concealed} concealed"
    )
    return "\n".join(lines)


async def handle_command(app: ConferenceApp, line: str) -> bool:
    """Run one command line; returns False when the client should exit."""
    line = line.strip()
    if not line:
        return True
    if line.startswith("send:"):
        line = "chat " + line[5:]
    cmd, _, arg = line.partition(" ")
    arg = arg.strip()
    try:
        if cmd in ("help", "?"):
            app.out(HELP)
        elif cmd == "list":
            rooms = await app.list_rooms()
            if not rooms:
                app.out("no open meetings")
            for r in rooms:
                app.out(
                    f"  {r['room_id']:>4}  {r['title']}  (host {r['host_name']}, "
                    f"{r['participants']}/{r['max_participants']}, {r['mode']})"
                )
        elif cmd == "create":
            j = await app.create(arg)
            app.out(f"* created meeting {j['room_id']} ({j['title']})")
        elif cmd == "join":
            j = await app.join(int(arg))
            app.out(f"* joined meeting {j['room_id']} ({j['title']})")
        elif cmd in ("leave", "quit"):
            await app.leave()
            app.out("* left the meeting")
        elif cmd == "cancel":
            await app.cancel()
            app.out("* meeting cancelled")
        elif cmd in ("chat", "say"):
            await app.chat(arg)
        elif cmd in ("video", "audio", "switch"):
            kind, value = (arg, None) if cmd == "switch" else (cmd, arg)
            current = app.video_on if kind == "video" else app.audio_on
            on = (not current) if value in (None, "") else value == "on"
            if kind == "video":
                await app.set_video(on)
            elif kind == "audio":
                await app.set_audio(on)
            else:
                raise ValueError("usage: switch video|audio")
            app.out(f"* {kind} {'on' if on else 'off'}")
        elif cmd == "status":
            app.out(_status(app))
        elif cmd == "sleep":
            await asyncio.sleep(float(arg))
        elif cmd == "exit":
            return False
        else:
            app.out(f"unknown command {cmd!r}; type 'help'")
    except ServerError as exc:
        app.out(f"! {exc.code}: {exc.message}")
    except (ValueError, TimeoutError, ConnectionError) as exc:
        app.out(f"! {exc}")
    return True


def _say(text: str) -> None:
    print(text, flush=True)


def _stdin_lines(loop: asyncio.AbstractEventLoop, put: Callable[[str | None], None]) -> None:
    """Read stdin on a thread (portable, unlike add_reader on Windows)."""
    for line in sys.stdin:
        loop.call_soon_threadsafe(put, line)
    loop.call_soon_threadsafe(put, None)


async def _next_line(lines: asyncio.Queue[str | None], closed: asyncio.Event) -> str | None:
    """The next stdin line, or None at end of input or when the server connection closed."""
    get = asyncio.ensure_future(lines.get())
    stop = asyncio.ensure_future(closed.wait())
    done, pending = await asyncio.wait({get, stop}, return_when=asyncio.FIRST_COMPLETED)
    for task in pending:
        task.cancel()
    return get.result() if get in done else None


async def run(args: argparse.Namespace) -> int:
    window = not args.headless
    if window and importlib.util.find_spec("cv2") is None:
        print("opencv-python is not installed: running without a window")
        window = False
    app = ConferenceApp(config_from_args(args), make_devices(args), out=_say, window=window)
    try:
        welcome = await app.start()
    except (OSError, TimeoutError) as exc:
        print(f"cannot connect to {args.server}:{args.port}: {exc}")
        return 1
    print(f"connected as {args.name} (client {welcome['client_id']}); type 'help' for commands", flush=True)
    try:
        startup = []
        if args.create is not None:
            startup.append(f"create {args.create}")
        elif args.join is not None:
            startup.append(f"join {args.join}")
        if args.script:
            startup.extend(args.script.split(";"))
        for command in startup:
            if not await handle_command(app, command):
                return 0
        if args.duration is not None:
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(app.control.closed.wait(), args.duration)
            return 0
        lines: asyncio.Queue[str | None] = asyncio.Queue()
        threading.Thread(
            target=_stdin_lines, args=(asyncio.get_running_loop(), lines.put_nowait), daemon=True
        ).start()
        while True:
            line = await _next_line(lines, app.control.closed)
            if line is None or not await handle_command(app, line):
                return 0
    finally:
        with contextlib.suppress(ServerError, ConnectionError, TimeoutError):
            if app.room is not None and not app.control.closed.is_set():
                await app.leave()
        await app.close()


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    with contextlib.suppress(KeyboardInterrupt):
        return asyncio.run(run(args))
    return 0
