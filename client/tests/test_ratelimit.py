from __future__ import annotations

import pytest

from confclient.ratelimit import TokenBucket

from .conftest import FakeClock


def test_burst_then_refill_at_rate() -> None:
    clock = FakeClock()
    bucket = TokenBucket(rate=1000, burst=500, clock=clock)
    assert bucket.try_consume(500)
    assert not bucket.try_consume(1)
    clock.advance(0.1)
    assert bucket.try_consume(100)
    assert not bucket.try_consume(1)


def test_refill_is_capped_at_burst() -> None:
    clock = FakeClock()
    bucket = TokenBucket(rate=1000, burst=500, clock=clock)
    bucket.try_consume(500)
    clock.advance(60)
    assert bucket.available() == 500


def test_whole_frame_or_nothing() -> None:
    """A request larger than the bucket is never admitted, and a refused request takes
    no tokens, so a frame that does not fit is dropped whole rather than in part."""
    clock = FakeClock()
    bucket = TokenBucket(rate=1000, burst=500, clock=clock)
    assert not bucket.try_consume(501)
    assert bucket.available() == 500


def test_long_run_average_matches_rate() -> None:
    clock = FakeClock()
    bucket = TokenBucket(rate=10_000, burst=2_000, clock=clock)
    sent = 0
    for _ in range(10_000):  # 10 s in 1 ms steps, offering 1000 B/ms (100x the rate)
        clock.advance(0.001)
        if bucket.try_consume(1000):
            sent += 1000
    assert 100_000 <= sent <= 102_000


def test_zero_rate_disables_limiting_and_negative_is_rejected() -> None:
    assert TokenBucket(rate=0, burst=0).try_consume(10**9)
    with pytest.raises(ValueError, match="must be >= 0"):
        TokenBucket(rate=-1, burst=0)
