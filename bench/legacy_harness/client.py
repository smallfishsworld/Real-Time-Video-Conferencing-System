"""One participant running the original client's media code (legacy/my_client.py).

The harness replaces only what cannot run here or what the original reads from a
person at a terminal:

* devices: stub modules (see stubs.py). The camera function that my_client imported
  from util is replaced by one that returns the benchmark's pre-encoded corpus frames,
  so every system sends byte-identical JPEGs and nobody pays for encoding.
* start-up: ConferenceClient.__init__ asks for a name on stdin and connects at once,
  so the harness builds the object with __new__, sets the attributes __init__ and
  start_conference would set, and performs the same create/join exchange with the
  MainServer and the same TCP connection to the ConferenceServer (from port
  20000 + ID). Each participant binds its sockets to its own loopback or veth address
  instead of 0.0.0.0, as if it ran on its own machine (the legacy relay skips every
  receiver whose IP equals the sender's IP).
* mode: `p2p` is set to False (relay), which is what auto_mode_switch picks for three
  or more members and what the `chgmode` command selects for two.

The media threads are the original ConferenceClient.keep_share and keep_recv methods.
The UDP socket is wrapped to log what is sent and received, `vid_da` is a dict that
logs every frame keep_recv stores, and the fake speaker logs the tag and the play-out
time of every audio chunk keep_recv writes.
"""

from __future__ import annotations

import argparse
import io
import json
import os
import socket
import sys
import threading
import time
from pathlib import Path
from typing import Any

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import stubs

from common import frame_index, load_frames, read_tags, tagged_pcm

CHUNK = 1024
TALKERS = 1  # set from --talkers


class Log:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.sent_video: list[tuple[int, float]] = []  # (frame index, send time)
        self.sent_audio: list[tuple[int, float]] = []  # (chunk seq, send time)
        self.recv_video: list[tuple[int, int, float]] = []  # (sender ID, frame index, time stored)
        self.played: list[tuple[int, int, float, int]] = []  # (talker, seq, play start, latency ms)
        self.chunks_played = 0
        self.rx = {"video": [0, 0], "audio": [0, 0], "other": [0, 0]}  # kind -> [packets, bytes]


LOG = Log()


def _kind(data: bytes) -> str:
    """Media kind of a legacy datagram: pickle protocol 4 of (ID, kind, bytes)."""
    try:
        off = 11 + (2 if data[11] == 0x4B else 3)  # BININT1 or BININT2 ID
        if data[off] == 0x4B:
            return {1: "video", 2: "audio"}.get(data[off + 1], "other")
    except IndexError:
        pass
    return "other"


class LoggingSocket:
    """The legacy UDP socket, with sendto/recvfrom logged."""

    def __init__(self, sock: socket.socket) -> None:
        self._sock = sock

    def sendto(self, data: bytes, addr: Any) -> int:
        t = time.monotonic()
        n = self._sock.sendto(data, addr)
        kind = _kind(data)
        if kind == "video":
            soi = data.find(b"\xff\xd8\xff\xfe", 0, 64)
            idx = frame_index(data[soi:]) if soi >= 0 else None
            if idx is not None:
                with LOG.lock:
                    LOG.sent_video.append((idx, t))
        elif kind == "audio":
            tags = read_tags(np.frombuffer(data[-4 - 2 * CHUNK : -4], dtype=np.int16), TALKERS)
            with LOG.lock:
                LOG.sent_audio.extend((seq, t) for _talker, seq, _t_ms in tags)
        return n

    def recvfrom(self, bufsize: int) -> tuple[bytes, Any]:
        data, addr = self._sock.recvfrom(bufsize)
        c = LOG.rx[_kind(data)]
        c[0] += 1
        c[1] += len(data)
        return data, addr

    def __getattr__(self, name: str) -> Any:
        return getattr(self._sock, name)


class LoggingFrames(dict[Any, Any]):
    """`vid_da`: keep_recv stores each decoded frame here under the sender's ID."""

    def __setitem__(self, key: Any, value: Any) -> None:
        t = time.monotonic()
        idx = None
        fp = getattr(value, "fp", None)
        if isinstance(fp, io.BytesIO):
            idx = frame_index(fp.getbuffer()[:64])
        if idx is not None:
            with LOG.lock:
                LOG.recv_video.append((int(key), idx, t))
        super().__setitem__(key, value)


class CorpusImage:
    """Stands in for the PIL image util.capture_camera returns; compress_image() calls
    save(), which writes the corpus frame's JPEG bytes."""

    def __init__(self, jpeg: bytes) -> None:
        self.jpeg = jpeg

    def save(self, fp: io.BytesIO, format: str = "JPEG", quality: int = 85) -> None:
        fp.write(self.jpeg)


def _send_field(sock: socket.socket, text: str) -> None:
    # The legacy servers read each 10-byte field with its own recv(); the original client
    # sends them back to back and relies on them arriving as separate segments. Pausing
    # between fields makes that reliable on a fast link.
    sock.sendall(text.rjust(10).encode())
    time.sleep(0.05)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--id", type=int, required=True, help="legacy user ID (also the name)")
    ap.add_argument("--ip", required=True, help="local address of this participant")
    ap.add_argument("--server-ip", required=True)
    ap.add_argument("--conf-id", type=int, default=1)
    ap.add_argument("--create", action="store_true")
    ap.add_argument("--send", action="store_true", help="run keep_share (send video and audio)")
    ap.add_argument("--talker", type=int, default=0, help="this talker's audio tag slot")
    ap.add_argument("--talkers", type=int, default=1, help="number of reserved audio tag slots")
    ap.add_argument("--end-at", type=float, required=True, help="time.monotonic() at which to stop")
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()
    global TALKERS
    TALKERS = args.talkers

    freq = 300.0 + 100 * args.talker

    def mic(k: int, due: float) -> bytes:
        pcm = tagged_pcm(CHUNK, args.talker, args.talkers, k, int(due * 1000) & 0xFFFFFFFF, freq, k * CHUNK)
        return pcm.tobytes()  # PyAudio paInt16 delivers native-endian samples

    def speaker(data: bytes, start: float) -> None:
        pcm = np.frombuffer(data, dtype=np.int16)
        start_ms = int(start * 1000)
        with LOG.lock:
            LOG.chunks_played += 1
            for talker, seq, t_ms in read_tags(pcm, args.talkers):
                LOG.played.append((talker, seq, start, (start_ms - t_ms) & 0xFFFFFFFF))

    stubs.install(args.server_ip, audio_source=mic, audio_sink=speaker)
    import my_client  # type: ignore[import-not-found]

    frames = load_frames()
    counter = iter(range(1 << 30))
    my_client.capture_camera = lambda: CorpusImage(frames[next(counter) % len(frames)])

    # The MainServer exchange of create_conference / join_conference.
    name = str(args.id)
    main_conn = socket.create_connection((args.server_ip, my_client.MAIN_SERVER_PORT), timeout=10)
    for field in ("create" if args.create else "join", name, str(args.conf_id)):
        _send_field(main_conn, field)
    reply = main_conn.recv(1024).decode().strip()
    if not reply.startswith("200:"):
        sys.exit(f"legacy server refused: {reply!r}")

    c = my_client.ConferenceClient.__new__(my_client.ConferenceClient)
    threading.Thread.__init__(c, daemon=True)
    c.is_working = True
    c.ID = args.id
    c.conference_id = args.conf_id
    c.video_open = True
    c.audio_open = True
    c.p2p = False
    c.users = {0}
    c.users_conns = {}
    c.is_meeting_creator = args.create
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 100000)  # as ConferenceClient.__init__
    udp.bind((args.ip, args.id * 100 + 20000))
    c.soc = LoggingSocket(udp)

    # start_conference: TCP to the ConferenceServer from port 20000 + ID, then media.
    c.cfconn = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    c.cfconn.bind((args.ip, args.id + 20000))
    c.cfconn.connect((args.server_ip, args.conf_id + 10000))
    c.vid_da = LoggingFrames()
    c.cfdaconn = (args.server_ip, args.conf_id * 100 + 10000)
    c.on_meeting = True

    def drain_control() -> None:
        while True:
            try:
                if not c.cfconn.recv(1024):
                    return
            except OSError:
                return

    threading.Thread(target=drain_control, daemon=True).start()
    threading.Thread(target=c.keep_recv, name="keep_recv", daemon=True).start()
    if args.send:
        threading.Thread(target=c.keep_share, name="keep_share", daemon=True).start()
    print(f"legacy client {args.id} in conference {args.conf_id} ({reply})", flush=True)

    while time.monotonic() < args.end_at:
        time.sleep(0.05)
    with LOG.lock:
        result = {
            "system": "legacy",
            "id": args.id,
            "talker": args.talker,
            "sent_video": LOG.sent_video,
            "sent_audio": LOG.sent_audio,
            "recv_video": LOG.recv_video,
            "played": LOG.played,
            "chunks_played": LOG.chunks_played,
            "rx": LOG.rx,
        }
    args.out.write_text(json.dumps(result))
    sys.stdout.flush()
    os._exit(0)  # the original threads never return


if __name__ == "__main__":
    main()
