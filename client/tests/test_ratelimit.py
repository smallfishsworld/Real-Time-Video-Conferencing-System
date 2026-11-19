from __future__ import annotations

import math

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


@pytest.mark.parametrize("rate", [0, -1, math.nan])
def test_a_rate_of_zero_negative_or_nan_is_rejected(rate: float) -> None:
    """0 used to mean "no limit", so `--max-send-kbps 0` silently removed the cap
    (review finding client-1). Only math.inf turns the limit off now."""
    with pytest.raises(ValueError, match="rate must be > 0"):
        TokenBucket(rate=rate, burst=0)


@pytest.mark.parametrize("burst", [-1, math.nan])
def test_a_negative_or_nan_burst_is_rejected(burst: float) -> None:
    with pytest.raises(ValueError, match="burst >= 0"):
        TokenBucket(rate=1000, burst=burst)


def test_infinite_rate_is_unlimited() -> None:
    bucket = TokenBucket(rate=math.inf, burst=0)
    assert bucket.unlimited
    assert bucket.try_consume(10**12)
    assert bucket.available() == math.inf


def test_consume_goes_into_debt_that_is_repaid_first() -> None:
    clock = FakeClock()
    bucket = TokenBucket(rate=1000, burst=500, clock=clock)
    bucket.consume(700)  # 200 tokens of debt
    assert bucket.available() == -200
    assert not bucket.try_consume(1)
    clock.advance(0.2)  # repays the debt
    assert bucket.available() == pytest.approx(0, abs=1e-6)
    assert not bucket.try_consume(1)
    clock.advance(0.11)
    assert bucket.try_consume(100)


def test_grow_lets_a_request_larger_than_the_burst_through_after_a_refill() -> None:
    clock = FakeClock()
    bucket = TokenBucket(rate=1000, burst=500, clock=clock)
    assert not bucket.try_consume(800)
    bucket.grow(1600)
    assert bucket.burst == 1600
    assert not bucket.try_consume(800)  # the tokens build up; growing adds none
    clock.advance(0.31)
    assert bucket.try_consume(800)
    bucket.grow(100)  # never shrinks
    assert bucket.burst == 1600
