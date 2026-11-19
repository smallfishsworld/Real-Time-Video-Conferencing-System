"""The client's loss detection and NACK timing."""

from __future__ import annotations

from confclient.nack import NackTracker


def test_gap_is_requested_after_the_delay_then_retried() -> None:
    t = NackTracker(delay=0.005, retry=0.03, max_tries=3, max_age=1.0)
    for seq in (10, 11, 14):  # 12 and 13 are missing
        t.on_packet(seq, 0.0)
    assert t.due(0.001) == []
    assert t.due(0.006) == [12, 13]
    assert t.due(0.010) == []  # not yet time to retry
    t.on_packet(12, 0.020)  # the retransmission of 12 arrives
    assert t.due(0.040) == [13]
    assert t.due(0.071) == [13]
    assert t.due(0.110) == []  # three requests made; abandoned once the last retry is due
    assert t.missing() == 0
    assert (t.stats.requested, t.stats.recovered, t.stats.abandoned) == (4, 1, 1)


def test_late_or_repaired_packets_cancel_requests() -> None:
    t = NackTracker(delay=0.005)
    for seq in (1, 3, 2):  # 2 only reordered
        t.on_packet(seq, 0.0)
    assert t.due(0.01) == []
    assert t.stats.recovered == 0  # never requested, so not counted as recovered


def test_old_gaps_are_abandoned() -> None:
    t = NackTracker(delay=0.0, retry=10.0, max_age=0.25)
    t.on_packet(1, 0.0)
    t.on_packet(3, 0.0)
    assert t.due(0.0) == [2]
    assert t.due(0.3) == []
    assert t.stats.abandoned == 1


def test_wraps_and_skips_huge_jumps() -> None:
    t = NackTracker(delay=0.0, max_gap=128)
    t.on_packet(65534, 0.0)
    t.on_packet(1, 0.0)  # 65535 and 0 missing across the wrap
    assert t.due(0.0) == [65535, 0]
    t.on_packet(1000, 0.0)  # a jump of 999: not requested
    assert t.due(0.001) == []
    t.on_packet(1002, 0.0)
    assert t.due(0.002) == [1001]
