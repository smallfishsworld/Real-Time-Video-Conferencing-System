"""One participant running the v2 client (confclient.ConferenceApp) for B1 and B2.

Devices are benchmark versions of the fakes: a camera that sends the corpus frames, a
microphone whose 20 ms frames carry tags (common.tagged_pcm) and a speaker thread that
pulls one frame every 20 ms like a sound card and logs which talkers' tags it played
and how old they were. Everything else is the real client: control connection, media
session, FEC, frame assembler, jitter buffers and playout mixer.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import sys
import threading
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from confclient.app import ClientConfig, ConferenceApp, Devices
from confclient.assembler import VideoFrame
from confclient.control import ServerError
from confclient.devices import AudioCallback, AudioSink, AudioSource, PullFrame, _Pacer
from confclient.jitter import AudioPlayout
from confclient.media import MediaConfig
from confclient.protocol import AUDIO_FRAME_SECONDS, AUDIO_SAMPLES_PER_FRAME

from common import frame_index, load_frames, read_tags, tagged_pcm


class TaggedMicrophone(AudioSource):
    def __init__(self, talker: int, talkers: int) -> None:
        self.talker = talker
        self.talkers = talkers
        self.freq = 300.0 + 100 * talker
        self.sent: list[tuple[int, float]] = []  # (seq, capture time)
        self._task: asyncio.Task[None] | None = None

    def start(self, loop: asyncio.AbstractEventLoop, on_frame: AudioCallback) -> None:
        self._task = loop.create_task(self._run(on_frame))

    async def _run(self, on_frame: AudioCallback) -> None:
        t0 = time.monotonic()
        k = 0
        while True:
            k += 1
            delay = t0 + k * AUDIO_FRAME_SECONDS - time.monotonic()
            if delay > 0:
                await asyncio.sleep(delay)
            now = time.monotonic()
            pcm = tagged_pcm(
                AUDIO_SAMPLES_PER_FRAME,
                self.talker,
                self.talkers,
                k,
                int(now * 1000) & 0xFFFFFFFF,
                self.freq,
                k * AUDIO_SAMPLES_PER_FRAME,
            )
            on_frame(pcm.astype(">i2").tobytes(), now)
            self.sent.append((k, now))

    def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()


class RecordingSpeaker(AudioSink):
    """Pulls a frame every 20 ms on its own thread (the device clock)."""

    def __init__(self, playout: AudioPlayout, talkers: int) -> None:
        self.playout = playout
        self.talkers = talkers
        self.played: list[tuple[int, int, float, int]] = []  # (talker, seq, t, latency ms)
        self.ticks: list[tuple[float, str]] = []  # (t, played|concealed|silent)
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self, pull: PullFrame) -> None:
        self._thread = threading.Thread(target=self._run, args=(pull,), daemon=True)
        self._thread.start()

    def _run(self, pull: PullFrame) -> None:
        pacer = _Pacer(AUDIO_FRAME_SECONDS)
        while not self._stop.is_set():
            pacer.wait()
            played, concealed = self.playout.frames_played, self.playout.frames_concealed
            frame = pull()
            t = time.monotonic()
            if self.playout.frames_played != played:
                status = "played"
            elif self.playout.frames_concealed != concealed:
                status = "concealed"
            else:
                status = "silent"
            self.ticks.append((t, status))
            t_ms = int(t * 1000)
            for talker, seq, tag_ms in read_tags(frame, self.talkers):
                self.played.append((talker, seq, t, (t_ms - tag_ms) & 0xFFFFFFFF))

    def stop(self) -> None:
        self._stop.set()


class BenchApp(ConferenceApp):
    def __init__(self, *a: Any, **kw: Any) -> None:
        super().__init__(*a, **kw)
        self.recv_video: list[tuple[str, int, float, float]] = []

    def _on_video(self, participant_id: int, frame: VideoFrame) -> None:
        idx = frame_index(frame.data)
        if idx is not None:
            name = self._names().get(participant_id, str(participant_id))
            self.recv_video.append((name, idx, frame.completed, frame.first_arrival))
        super()._on_video(participant_id, frame)


async def video_loop(
    app: ConferenceApp, frames: list[bytes], fps: float, log: list[tuple[int, float, bool]]
) -> None:
    """Send corpus frames at `fps`, like ConferenceApp's camera loop, logging each send."""
    pacer_t0 = time.monotonic()
    k = 0
    while True:
        due = pacer_t0 + k / fps
        delay = due - time.monotonic()
        if delay > 0:
            await asyncio.sleep(delay)
        if app.session is not None:
            t = time.monotonic()
            fid = app.session.send_video(frames[k % len(frames)], t)
            log.append((k % len(frames), t, fid is not None))
        k += 1


async def run(args: argparse.Namespace) -> dict[str, Any]:
    frames = load_frames() if args.send_video else []
    media = MediaConfig(
        video_fec_k=args.video_fec_k,
        audio_fec_k=args.audio_fec_k,
        audio_delay_frames=max(1, args.jitter_ms // 20),
        max_send_kbps=args.max_send_kbps,
    )
    mic = TaggedMicrophone(args.talker, args.talkers) if args.send_audio else None
    speakers: list[RecordingSpeaker] = []
    app: BenchApp

    def speaker() -> AudioSink:
        s = RecordingSpeaker(app.playout, args.talkers)
        speakers.append(s)
        return s

    devices = Devices(video=None, microphone=(lambda: mic) if mic else None, speaker=speaker)
    app = BenchApp(
        ClientConfig(name=args.name, server=args.server, port=args.port, media=media), devices, out=print
    )
    await app.start()
    if args.create is not None:
        await app.create(args.create)
    else:
        deadline = time.monotonic() + 10
        while True:
            try:
                await app.join(args.join)
                break
            except ServerError as exc:
                if exc.code != "room_not_found" or time.monotonic() > deadline:
                    raise
                await asyncio.sleep(0.1)
    sent_video: list[tuple[int, float, bool]] = []
    task = asyncio.create_task(video_loop(app, frames, args.fps, sent_video)) if args.send_video else None
    with contextlib.suppress(TimeoutError):
        await asyncio.wait_for(app.room_closed.wait(), max(0.0, args.end_at - time.monotonic()))
    if task is not None:
        task.cancel()
    session = app.session
    stats = session.stats if session is not None else None
    result: dict[str, Any] = {
        "system": "v2",
        "name": args.name,
        "talker": args.talker,
        "sent_video": sent_video,
        "sent_audio": mic.sent if mic else [],
        "recv_video": app.recv_video,
        "played": speakers[0].played if speakers else [],
        "ticks": speakers[0].ticks if speakers else [],
        "rx_packets": stats.packets_in if stats else {},
        "rx_bytes": stats.bytes_in if stats else {},
        "fec_recovered": stats.fec_recovered if stats else 0,
        "video_frames_rate_limited": stats.video_frames_rate_limited if stats else 0,
        "audio_frames_rate_limited": stats.audio_frames_rate_limited if stats else 0,
        "mode": session.mode if session else None,
        "jitter": {
            str(ssrc): {"received": s.received, "played": s.played, "concealed": s.concealed, "late": s.late}
            for ssrc, s in app.playout.stream_stats().items()
        },
    }
    with contextlib.suppress(ServerError, ConnectionError, TimeoutError):
        await app.leave()
    await app.close()
    return result


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--server", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=6100)
    ap.add_argument("--name", required=True)
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--create", metavar="TITLE")
    g.add_argument("--join", type=int, metavar="ROOM")
    ap.add_argument("--send-video", action="store_true")
    ap.add_argument("--send-audio", action="store_true")
    ap.add_argument("--fps", type=float, default=15.625, help="legacy rate: one frame per 1024-sample chunk")
    ap.add_argument("--talker", type=int, default=0)
    ap.add_argument("--talkers", type=int, default=1)
    ap.add_argument("--video-fec-k", type=int, default=4)
    ap.add_argument("--audio-fec-k", type=int, default=2)
    ap.add_argument("--jitter-ms", type=int, default=60)
    ap.add_argument("--max-send-kbps", type=int, default=6000)
    ap.add_argument("--end-at", type=float, required=True, help="time.monotonic() at which to stop")
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()
    result = asyncio.run(run(args))
    args.out.write_text(json.dumps(result))


if __name__ == "__main__":
    main()
