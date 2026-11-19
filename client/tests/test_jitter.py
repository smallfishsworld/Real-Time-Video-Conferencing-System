from __future__ import annotations

from collections.abc import Callable

import numpy as np
import pytest

from confclient.jitter import AudioJitterBuffer, AudioPlayout, Frame, PlayStatus, decode_l16, encode_l16
from confclient.protocol import AUDIO_SAMPLES_PER_FRAME

TS0 = 0xFFFFF000  # close to the 32-bit wrap, so every test also covers wrap-around
N = AUDIO_SAMPLES_PER_FRAME


def _frame(value: int) -> bytes:
    return encode_l16(np.full(N, value, dtype=np.int16))


def _ts(i: int) -> int:
    return (TS0 + i * N) & 0xFFFFFFFF


def _values(results: list[tuple[Frame | None, PlayStatus]]) -> list[int | None]:
    return [None if f is None else int(f[0]) for f, _ in results]


def test_l16_is_big_endian() -> None:
    frame = np.array([1, -2], dtype=np.int16)
    assert encode_l16(frame) == b"\x00\x01\xff\xfe"
    assert decode_l16(b"\x00\x01\xff\xfe").tolist() == [1, -2]


def test_steady_stream_plays_after_fixed_delay() -> None:
    jb = AudioJitterBuffer(delay_frames=3)
    out = []
    for i in range(10):
        assert jb.push(_ts(i), _frame(100 + i))
        out.append(jb.pop())
    assert _values(out) == [None, None, None, 100, 101, 102, 103, 104, 105, 106]
    assert [s for _, s in out[3:]] == [PlayStatus.PLAYED] * 7


def test_reordering_within_the_delay_is_hidden() -> None:
    jb = AudioJitterBuffer(delay_frames=3)
    for i in (0, 2, 1, 3):
        jb.push(_ts(i), _frame(i + 1))
    out = [jb.pop() for _ in range(7)]
    assert _values(out)[3:] == [1, 2, 3, 4]


def test_late_and_duplicate_packets_are_dropped() -> None:
    jb = AudioJitterBuffer(delay_frames=1)
    jb.push(_ts(0), _frame(1))
    jb.pop()
    assert jb.pop()[0] is not None  # frame 0 played
    assert not jb.push(_ts(0), _frame(1))  # late
    assert jb.push(_ts(2), _frame(3))
    assert not jb.push(_ts(2), _frame(3))  # duplicate
    assert jb.stats.late == 1
    assert jb.stats.duplicates == 1


def test_loss_is_concealed_with_halving_repeats_then_silence() -> None:
    jb = AudioJitterBuffer(delay_frames=1)
    out = []
    for i in range(12):
        if not 4 <= i < 9:  # frames 4..8 are lost
            jb.push(_ts(i), _frame(800))
        out.append(jb.pop())
    statuses = [s for _, s in out[1:]]
    assert statuses[:3] == [PlayStatus.PLAYED] * 3
    assert statuses[3:8] == [PlayStatus.PLAYED] + [PlayStatus.CONCEALED] * 3 + [PlayStatus.SILENT]
    assert _values(out)[5:8] == [400, 200, 100]
    assert jb.stats.concealed == 3


def test_latency_is_bounded_when_the_sender_runs_ahead() -> None:
    jb = AudioJitterBuffer(delay_frames=3)
    jb.push(_ts(0), _frame(1))
    jb.push(_ts(30), _frame(31))  # a burst far ahead of the playout cursor
    pops = 0
    while jb.pop()[0] is None:
        pops += 1
        assert pops < 10, "the frame far ahead must play within delay + 5 frames"
    assert jb.stats.skipped == 1  # frame 0 had to be skipped to catch up


def test_resync_after_idle_starts_a_new_talk_spurt_cleanly() -> None:
    jb = AudioJitterBuffer(delay_frames=2)
    jb.push(_ts(0), _frame(5))
    for _ in range(40):  # silence long enough to reset (500 ms)
        jb.pop()
    assert jb.idle_ticks() >= 25
    jb.push(_ts(1000), _frame(9))
    out = [jb.pop() for _ in range(3)]
    # No concealment with audio from before the pause, then the new spurt plays.
    assert [s for _, s in out] == [PlayStatus.SILENT, PlayStatus.SILENT, PlayStatus.PLAYED]


def test_misaligned_timestamp_resynchronises() -> None:
    jb = AudioJitterBuffer(delay_frames=1)
    jb.push(_ts(0), _frame(1))
    jb.push(_ts(5) + 7, _frame(2))  # not a multiple of 320 samples from the cursor
    out = [jb.pop() for _ in range(2)]
    assert _values(out) == [None, 2]


def _simulate(
    period_us: int,
    frames: int,
    extra_delay: Callable[[int], int] = lambda k: 0,
    burst: int = 0,
    delay_frames: int = 3,
) -> tuple[int, list[float], AudioJitterBuffer]:
    """Frame k is sent at k * period_us and arrives 30 ms (plus extra_delay(k)) later; the
    first `burst` frames arrive together. Every 20 ms, 7 ms after the send grid, the
    arrived frames are pushed and one frame is popped. Returns the longest run of
    unplayed ticks while the sender is active (ms), each played frame's latency from
    send to playout (ms) and the buffer."""
    send = [k * period_us for k in range(frames)]
    arrive = [send[k] + 30_000 + extra_delay(k) for k in range(frames)]
    for k in range(burst - 1):
        arrive[k] = arrive[burst - 1]
    order = sorted(range(frames), key=lambda k: arrive[k])
    jb = AudioJitterBuffer(delay_frames)
    latencies: list[float] = []
    longest = run = 0
    started = False
    ai = 0
    t = 7000
    while t <= send[-1]:
        while ai < frames and arrive[order[ai]] <= t:
            k = order[ai]
            jb.push((k * N) & 0xFFFFFFFF, encode_l16(np.full(N, k % 30000, dtype=np.int16)))
            ai += 1
        frame, status = jb.pop()
        if status is PlayStatus.PLAYED:
            assert frame is not None
            started = True
            latencies.append((t - send[int(frame[0])]) / 1000)
            run = 0
        elif started:
            run += 1
            longest = max(longest, run * 20)
        t += 20_000
    return longest, latencies, jb


# The default 3-frame buffer at a 30 ms one-way delay plays each frame 107 ms after it
# was sent: 30 ms, plus 60 ms of buffer, plus 17 ms until the next tick.
TARGET_MS = 107.0


@pytest.mark.parametrize("period_us", [20_020, 19_980])
def test_sender_clock_drift_causes_neither_gaps_nor_latency_growth(period_us: int) -> None:
    # 1000 ppm for five minutes is 15 frames of drift. Before, a slow sender lost
    # 560 ms of audio about once a minute and a fast one ended at 200 ms.
    longest, lat, jb = _simulate(period_us, 15_000)
    assert longest <= 20
    assert jb.stats.late == 0
    tail = lat[-500:]
    assert min(tail) >= TARGET_MS - 20
    assert max(tail) <= TARGET_MS + 20
    assert jb.stats.stretched + jb.stats.drained == 15


def test_start_up_burst_drains_back_to_the_target() -> None:
    longest, lat, jb = _simulate(20_000, 3000, burst=50)
    assert longest == 0
    assert lat[-500:] == [TARGET_MS] * 500  # before: 207 ms for the rest of the call
    assert jb.stats.drained > 0


@pytest.mark.parametrize(("step_ms", "max_gap_ms"), [(60, 60), (100, 120), (-100, 0)])
def test_delay_steps_are_followed_without_long_gaps(step_ms: int, max_gap_ms: int) -> None:
    if step_ms > 0:
        longest, lat, _ = _simulate(20_000, 3000, lambda k: step_ms * 1000 if k >= 1000 else 0)
    else:
        longest, lat, _ = _simulate(20_000, 3000, lambda k: -step_ms * 1000 if k < 1000 else 0)
    # Before, a +100 ms step left 28 ticks (560 ms) unplayed until the idle resync.
    assert longest <= max_gap_ms
    assert lat[-500:] == [TARGET_MS + max(step_ms, 0)] * 500


def test_a_run_of_late_packets_reanchors_and_keeps_them() -> None:
    jb = AudioJitterBuffer(delay_frames=1)
    jb.push(_ts(0), _frame(1))
    for _ in range(5):  # frame 0 plays at the second pop; the cursor is now at frame 4
        jb.pop()
    # Frames 1..3 arrive after their playout time: the third re-anchors the buffer
    # one frame behind it, and the one before it is kept.
    assert not jb.push(_ts(1), _frame(2))
    assert not jb.push(_ts(2), _frame(3))
    assert jb.push(_ts(3), _frame(4))
    assert jb.stats.reanchored == 1
    assert _values([jb.pop(), jb.pop()]) == [3, 4]


def test_rejects_wrong_frame_size() -> None:
    jb = AudioJitterBuffer()
    assert not jb.push(0, b"\x00" * 10)
    with pytest.raises(ValueError, match="delay_frames"):
        AudioJitterBuffer(delay_frames=0)


def test_playout_sums_streams_and_saturates() -> None:
    playout = AudioPlayout(delay_frames=1)
    seen: list[Frame] = []
    playout.observer = lambda frame, _t: seen.append(frame)
    playout.push(1, _ts(0), _frame(1000))
    playout.push(2, _ts(77), _frame(2000))  # streams have independent timestamps
    playout.pull()
    assert int(playout.pull()[0]) == 3000
    playout.push(1, _ts(1), _frame(30000))
    playout.push(2, _ts(78), _frame(30000))
    assert int(playout.pull()[0]) == 32767
    assert playout.frames_played == 2
    assert len(seen) == 3
    assert set(playout.stream_stats()) == {1, 2}


def test_playout_forgets_idle_streams() -> None:
    playout = AudioPlayout(delay_frames=1, idle_timeout=-1.0)
    playout.push(1, 0, _frame(1))
    assert not playout.pull().any()
    assert playout.stream_stats() == {}
