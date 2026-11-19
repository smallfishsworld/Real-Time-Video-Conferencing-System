"""Loss detection and retransmission requests (generic NACK) for received video."""

from __future__ import annotations

from dataclasses import dataclass, field

from .protocol import seq_diff


@dataclass(slots=True)
class _Missing:
    since: float  # when a later packet showed this one missing
    asked: float = 0.0  # when it was last requested
    tries: int = 0


@dataclass(slots=True)
class NackStats:
    requested: int = 0  # sequence numbers requested, counting repeats
    recovered: int = 0  # requested packets that arrived afterwards
    abandoned: int = 0  # still missing after max_tries requests or max_age


@dataclass
class NackTracker:
    """Follows one sender's RTP sequence numbers and decides which to request again.

    A packet is missing once a later sequence number has arrived. It is requested
    `delay` seconds later (by then reordering or FEC has usually repaired it), then
    again every `retry` seconds, at most `max_tries` times and for at most `max_age`
    seconds, after which the frame it belongs to is past its use. A jump of more than
    `max_gap` sequence numbers (a sender restart or a long outage) is not requested
    at all; the server keeps only a few hundred packets per sender anyway.
    """

    delay: float = 0.005
    retry: float = 0.03
    max_tries: int = 3
    max_age: float = 0.25
    max_gap: int = 128
    stats: NackStats = field(default_factory=NackStats)
    _top: int | None = None
    _missing: dict[int, _Missing] = field(default_factory=dict)

    def on_packet(self, seq: int, now: float) -> None:
        """Record a packet that arrived (or that FEC rebuilt)."""
        if self._top is None:
            self._top = seq
            return
        d = seq_diff(seq, self._top)
        if d > 0:
            if d - 1 <= self.max_gap:
                for k in range(1, d):
                    self._missing.setdefault((self._top + k) & 0xFFFF, _Missing(now))
            self._top = seq
            return
        m = self._missing.pop(seq, None)
        if m is not None and m.tries > 0:
            self.stats.recovered += 1

    def due(self, now: float) -> list[int]:
        """The sequence numbers to request now, oldest first."""
        out: list[int] = []
        for seq, m in list(self._missing.items()):
            if now - m.since > self.max_age or (m.tries >= self.max_tries and now - m.asked >= self.retry):
                del self._missing[seq]
                self.stats.abandoned += 1
            elif (m.tries == 0 and now - m.since >= self.delay) or (
                0 < m.tries < self.max_tries and now - m.asked >= self.retry
            ):
                m.tries += 1
                m.asked = now
                out.append(seq)
        self.stats.requested += len(out)
        top = self._top if self._top is not None else 0
        out.sort(key=lambda s: seq_diff(s, top))
        return out

    def missing(self) -> int:
        return len(self._missing)
