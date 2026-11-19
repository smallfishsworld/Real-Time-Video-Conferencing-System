"""Audio playout: a fixed-delay jitter buffer per incoming stream with packet-loss
concealment, and a playout mixer that sums all incoming streams.

The audio device thread pulls one 20 ms frame per tick (the device is the master
clock); the network thread pushes packets. Both sides take a lock.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum

import numpy as np
import numpy.typing as npt

from .protocol import AUDIO_FRAME_BYTES, AUDIO_SAMPLES_PER_FRAME

Frame = npt.NDArray[np.int16]

_SLOTS = 64  # 1.28 s of buffer at most
_MAX_EXTRA = 5  # frames above the target delay before old frames are skipped at once
_IDLE_RESET = 25  # empty ticks (500 ms) before the stream re-synchronises
_PLC_FRAMES = 3  # concealed frames before falling silent
_LATE_RESYNC = 3  # late packets in a row before the buffer re-anchors on the newest
_WINDOW = 25  # ticks (500 ms) per buffer-level check


class PlayStatus(Enum):
    PLAYED = "played"
    CONCEALED = "concealed"
    SILENT = "silent"


@dataclass(slots=True)
class JitterStats:
    received: int = 0
    played: int = 0
    concealed: int = 0
    late: int = 0
    skipped: int = 0
    duplicates: int = 0
    drained: int = 0  # frames dropped because the level stayed above the target
    stretched: int = 0  # concealed frames inserted because it stayed below
    reanchored: int = 0  # re-anchors after a run of late packets


def decode_l16(pcm_be: bytes | memoryview) -> Frame:
    """Big-endian L16 payload to a native int16 array."""
    return np.frombuffer(pcm_be, dtype=">i2").astype(np.int16)


def encode_l16(frame: Frame) -> bytes:
    return frame.astype(">i2").tobytes()


class AudioJitterBuffer:
    """Playout buffer with a fixed target delay for one 20 ms L16 stream.

    The first packet sets the playout cursor `delay_frames` frames behind it. Each pop
    advances the cursor by one frame. A missing frame is concealed by repeating the
    last frame at half the amplitude each time, for at most three frames.

    The buffer holds the latency at the target as the sender's clock or the network
    delay changes. Every `_WINDOW` ticks it compares its level (the frames it holds
    beyond the cursor) with the target: a window in which the level never fell to the
    target drops one frame (a faster sender clock, or a delay that fell), and a window
    in which it never reached the target plays one concealed frame without advancing
    (a slower clock). Late packets are dropped, but `_LATE_RESYNC` of them in a row
    mean the delay has grown: the buffer re-anchors on the newest and keeps them.
    A burst more than `_MAX_EXTRA` frames beyond the target skips the oldest at once.
    """

    def __init__(self, delay_frames: int = 3) -> None:
        if not 1 <= delay_frames < _SLOTS - _MAX_EXTRA:
            raise ValueError("delay_frames out of range")
        self.delay = delay_frames
        self.stats = JitterStats()
        self._lock = threading.Lock()
        self._slots: list[tuple[int, Frame] | None] = [None] * _SLOTS
        self._started = False
        self._cursor = 0
        self._head = 0
        self._idle = 0
        self._last: Frame | None = None
        self._plc = 0
        self._newest = 0  # highest timestamp stored since the last resync
        self._late: list[tuple[int, Frame]] = []  # the current run of late frames
        self._win = 0
        self._win_min = _SLOTS
        self._win_max = -_SLOTS

    def push(self, timestamp: int, pcm_be: bytes | memoryview) -> bool:
        """Store one frame; returns False if it was too late or malformed."""
        if len(pcm_be) != AUDIO_FRAME_BYTES:
            return False
        frame = decode_l16(pcm_be)
        with self._lock:
            self.stats.received += 1
            if not self._started:
                self._resync(timestamp)
            d = (timestamp - self._cursor) & 0xFFFFFFFF
            if d >= 0x80000000:
                return self._push_late(timestamp, frame)
            self._late.clear()
            if d % AUDIO_SAMPLES_PER_FRAME or d // AUDIO_SAMPLES_PER_FRAME >= _SLOTS:
                self._resync(timestamp)
                d = self.delay * AUDIO_SAMPLES_PER_FRAME
            if 0 < (timestamp - self._newest) & 0xFFFFFFFF < 0x80000000:
                self._newest = timestamp
            idx = d // AUDIO_SAMPLES_PER_FRAME
            skip = idx - self.delay - _MAX_EXTRA
            for _ in range(max(0, skip)):
                if self._slots[self._head] is not None:
                    self.stats.skipped += 1
                self._slots[self._head] = None
                self._head = (self._head + 1) % _SLOTS
                self._cursor = (self._cursor + AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF
            idx -= max(0, skip)
            slot = (self._head + idx) % _SLOTS
            existing = self._slots[slot]
            if existing is not None and existing[0] == timestamp:
                self.stats.duplicates += 1
                return False
            self._slots[slot] = (timestamp, frame)
            return True

    def _push_late(self, timestamp: int, frame: Frame) -> bool:
        """A frame whose playout time has passed; lock held."""
        self._late.append((timestamp, frame))
        if len(self._late) < _LATE_RESYNC:
            self.stats.late += 1
            return False
        run, last = self._late, self._last
        self._resync(timestamp)
        self._last = last  # conceal across the jump as across a loss
        for ts, f in run:
            d = (ts - self._cursor) & 0xFFFFFFFF
            if d % AUDIO_SAMPLES_PER_FRAME == 0 and d // AUDIO_SAMPLES_PER_FRAME < _SLOTS:
                self._slots[(self._head + d // AUDIO_SAMPLES_PER_FRAME) % _SLOTS] = (ts, f)
        self.stats.reanchored += 1
        return True

    def _level(self) -> int:
        """Track the frames held beyond the cursor; once per window say whether to drop
        a frame (+1), repeat one (-1) or do nothing (0). Lock held."""
        d = (self._newest - self._cursor) & 0xFFFFFFFF
        lv = (d - (1 << 32) if d >= 0x80000000 else d) // AUDIO_SAMPLES_PER_FRAME
        self._win_min = min(self._win_min, lv)
        self._win_max = max(self._win_max, lv)
        self._win += 1
        if self._win < _WINDOW:
            return 0
        lo, hi = self._win_min, self._win_max
        self._reset_window()
        if lo > self.delay:
            return 1
        if hi < self.delay:
            return -1
        return 0

    def _reset_window(self) -> None:
        self._win, self._win_min, self._win_max = 0, _SLOTS, -_SLOTS

    def pop(self) -> tuple[Frame | None, PlayStatus]:
        with self._lock:
            if not self._started:
                return None, PlayStatus.SILENT
            adjust = self._level()
            if adjust > 0:  # drop the frame due now and play the next one
                self._slots[self._head] = None
                self._head = (self._head + 1) % _SLOTS
                self._cursor = (self._cursor + AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF
                self.stats.drained += 1
            elif adjust < 0:  # stretch: conceal one frame, play the due one next tick
                due = self._slots[self._head]
                if self._last is not None and due is not None and due[0] == self._cursor:
                    self.stats.stretched += 1
                    return (self._last >> 1).astype(np.int16), PlayStatus.CONCEALED
            entry = self._slots[self._head]
            self._slots[self._head] = None
            due = self._cursor
            self._head = (self._head + 1) % _SLOTS
            self._cursor = (self._cursor + AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF
            if entry is not None and entry[0] == due:
                self._last = entry[1]
                self._plc = 0
                self._idle = 0
                self.stats.played += 1
                return entry[1], PlayStatus.PLAYED
            self._idle += 1
            if self._idle >= _IDLE_RESET:
                self._started = False
                self._last = None
            if self._last is not None and self._plc < _PLC_FRAMES:
                self._plc += 1
                self.stats.concealed += 1
                return (self._last >> self._plc).astype(np.int16), PlayStatus.CONCEALED
            return None, PlayStatus.SILENT

    def idle_ticks(self) -> int:
        with self._lock:
            return self._idle if self._started else _IDLE_RESET

    def _resync(self, timestamp: int) -> None:
        self._started = True
        self._cursor = (timestamp - self.delay * AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF
        self._newest = timestamp
        self._head = 0
        self._idle = 0
        self._slots = [None] * _SLOTS
        self._last = None
        self._plc = 0
        self._late = []
        self._reset_window()


FrameObserver = Callable[[Frame, float], None]


class AudioPlayout:
    """Sums every incoming audio stream into the frames the speaker plays.

    In relay mode there is one stream (the server's mix); in P2P mode it is the peer's
    stream; during a mode switch both may briefly exist, and summing them is correct
    because each talker's audio travels on exactly one path at a time.
    """

    def __init__(self, delay_frames: int = 3, idle_timeout: float = 5.0) -> None:
        self.delay = delay_frames
        self.idle_timeout = idle_timeout
        self._lock = threading.Lock()
        self._streams: dict[int, AudioJitterBuffer] = {}
        self._last_push: dict[int, float] = {}
        self.observer: FrameObserver | None = None
        self.frames_played = 0
        self.frames_concealed = 0

    def push(self, ssrc: int, timestamp: int, pcm_be: bytes | memoryview) -> None:
        with self._lock:
            jb = self._streams.get(ssrc)
            if jb is None:
                jb = AudioJitterBuffer(self.delay)
                self._streams[ssrc] = jb
            self._last_push[ssrc] = time.monotonic()
        jb.push(timestamp, pcm_be)

    def stream_stats(self) -> dict[int, JitterStats]:
        with self._lock:
            return {ssrc: jb.stats for ssrc, jb in self._streams.items()}

    def pull(self) -> Frame:
        """Return the next 20 ms frame (native int16); silence when nothing is due."""
        now = time.monotonic()
        with self._lock:
            for ssrc in [s for s, t in self._last_push.items() if now - t > self.idle_timeout]:
                del self._streams[ssrc]
                del self._last_push[ssrc]
            streams = list(self._streams.values())
        total = np.zeros(AUDIO_SAMPLES_PER_FRAME, dtype=np.int32)
        played = concealed = False
        for jb in streams:
            frame, status = jb.pop()
            if frame is not None:
                total += frame
                played |= status is PlayStatus.PLAYED
                concealed |= status is PlayStatus.CONCEALED
        if played:
            self.frames_played += 1
        elif concealed:
            self.frames_concealed += 1
        out = np.clip(total, -32768, 32767).astype(np.int16)
        if self.observer is not None:
            self.observer(out, now)
        return out
