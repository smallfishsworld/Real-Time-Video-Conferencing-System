"""Token bucket used to cap the client's send bitrate."""

from __future__ import annotations

import math
import time
from collections.abc import Callable


class TokenBucket:
    """Rate limiter: tokens refill at `rate` per second up to `burst`.

    `try_consume(n)` spends n tokens if they are available. It never blocks, so a media
    sender that runs out of budget drops the frame instead of queueing it (a late
    frame is worthless in real-time media). `consume(n)` spends tokens unconditionally
    (into debt if need be), for packets that must go out but should still count
    against the budget: the debt is paid back before anything else passes, so the
    long-run rate stays at `rate`. `grow(burst)` raises the burst, for a request that
    would otherwise never fit.

    `rate` must be positive; math.inf means no limit. There is no other way to turn
    the limit off.
    """

    def __init__(self, rate: float, burst: float, clock: Callable[[], float] = time.monotonic) -> None:
        if not rate > 0 or not burst >= 0:  # also rejects NaN
            raise ValueError("rate must be > 0 (math.inf for no limit) and burst >= 0")
        self.rate = rate
        self.burst = burst
        self._clock = clock
        self._tokens = burst
        self._last = clock()

    @property
    def unlimited(self) -> bool:
        return math.isinf(self.rate)

    def _refill(self) -> None:
        now = self._clock()
        elapsed = now - self._last
        if elapsed > 0:
            self._tokens = min(self.burst, self._tokens + elapsed * self.rate)
        self._last = now

    def available(self) -> float:
        if self.unlimited:
            return math.inf
        self._refill()
        return self._tokens

    def try_consume(self, n: float) -> bool:
        if self.unlimited:
            return True
        self._refill()
        if n > self._tokens:
            return False
        self._tokens -= n
        return True

    def consume(self, n: float) -> None:
        if not self.unlimited:
            self._refill()
            self._tokens -= n

    def grow(self, burst: float) -> None:
        """Raise the burst to at least `burst`; the tokens build up to it over time."""
        self._refill()
        self.burst = max(self.burst, burst)
