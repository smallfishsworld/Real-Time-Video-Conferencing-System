"""Reassembly of JPEG frames from RTP fragments."""

from __future__ import annotations

from dataclasses import dataclass, field

from .protocol import ProtocolError, RtpHeader, parse_video, u32_newer


@dataclass(slots=True)
class VideoFrame:
    ssrc: int
    frame_id: int
    timestamp: int
    data: bytes
    first_arrival: float
    completed: float
    first_seq: int = -1  # RTP sequence numbers of the first and the last fragment
    last_seq: int = -1


@dataclass(slots=True)
class AssemblerStats:
    completed: int = 0
    expired: int = 0  # incomplete when the deadline passed
    superseded: int = 0  # completed after a newer frame had already been shown
    duplicates: int = 0
    malformed: int = 0


@dataclass(slots=True)
class _Pending:
    count: int
    timestamp: int
    first_arrival: float
    parts: list[bytes | None]
    have: int = 0
    first_seq: int = -1
    last_seq: int = -1


@dataclass
class FrameAssembler:
    """Collects the fragments of one sender's frames.

    Fragments may arrive in any order and more than once. A frame is emitted once, as
    soon as all its fragments are present. JPEG frames are independent, so the policy
    is newest-wins: a frame that completes after a newer frame was emitted is dropped,
    and a frame still incomplete `deadline` seconds after its first fragment is
    abandoned.
    """

    deadline: float = 0.3
    max_pending: int = 32
    stats: AssemblerStats = field(default_factory=AssemblerStats)
    _pending: dict[int, _Pending] = field(default_factory=dict)
    _last_emitted: int | None = None

    def push(self, header: RtpHeader, payload: memoryview, now: float) -> VideoFrame | None:
        try:
            vh, frag = parse_video(payload)
        except ProtocolError:
            self.stats.malformed += 1
            return None
        fid = vh.frame_id
        if self._last_emitted is not None and not u32_newer(fid, self._last_emitted):
            self.stats.duplicates += 1
            return None
        p = self._pending.get(fid)
        if p is None:
            if len(self._pending) >= self.max_pending:
                self._drop_oldest()
            p = _Pending(
                count=vh.count, timestamp=header.timestamp, first_arrival=now, parts=[None] * vh.count
            )
            self._pending[fid] = p
        elif p.count != vh.count:
            self.stats.malformed += 1
            return None
        if p.parts[vh.index] is not None:
            self.stats.duplicates += 1
            return None
        p.parts[vh.index] = bytes(frag)
        p.have += 1
        if vh.index == 0:
            p.first_seq = header.sequence
        if vh.index == p.count - 1:
            p.last_seq = header.sequence
        if p.have < p.count:
            return None
        del self._pending[fid]
        self._discard_older(fid)
        self._last_emitted = fid
        self.stats.completed += 1
        data = b"".join(part for part in p.parts if part is not None)
        return VideoFrame(header.ssrc, fid, p.timestamp, data, p.first_arrival, now, p.first_seq, p.last_seq)

    def expire(self, now: float) -> int:
        """Abandon frames whose deadline passed; returns how many were dropped."""
        old = [fid for fid, p in self._pending.items() if now - p.first_arrival > self.deadline]
        for fid in old:
            del self._pending[fid]
        self.stats.expired += len(old)
        return len(old)

    def pending(self) -> int:
        return len(self._pending)

    def _discard_older(self, fid: int) -> None:
        older = [f for f in self._pending if u32_newer(fid, f)]
        for f in older:
            del self._pending[f]
        self.stats.superseded += len(older)

    def _drop_oldest(self) -> None:
        oldest = min(self._pending, key=lambda f: self._pending[f].first_arrival)
        del self._pending[oldest]
        self.stats.expired += 1
