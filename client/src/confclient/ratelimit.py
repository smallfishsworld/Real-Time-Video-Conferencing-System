"""Token bucket used to cap the client's send bitrate."""

from __future__ import annotations

import time
from collections.abc import Callable


class TokenBucket:
    """Byte-rate limiter: tokens refill at `rate` bytes per second up to `burst`.

    `try_consume(n)` spends n tokens if they are available. It never blocks, so a media
    sender that runs out of budget drops the frame instead of queueing it (a late
    frame is worthless in real-time media).
    """

    def __init__(self, rate: float, burst: float, clock: Callable[[], float] = time.monotonic) -> None:
        if rate < 0 or burst < 0:
            raise ValueError("rate and burst must be >= 0")
        self.rate = rate
        self.burst = burst
        self._clock = clock
        self._tokens = burst
        self._last = clock()

    def _refill(self) -> None:
        now = self._clock()
        elapsed = now - self._last
        if elapsed > 0:
            self._tokens = min(self.burst, self._tokens + elapsed * self.rate)
        self._last = now

    def available(self) -> float:
        self._refill()
        return self._tokens

    def try_consume(self, n: float) -> bool:
        if self.rate == 0:
            return True  # a rate of 0 disables limiting
        self._refill()
        if n > self._tokens:
            return False
        self._tokens -= n
        return True
