"""Checks of the benchmark analysis code in bench/, which computes the published numbers."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "bench"))

import analyze_b1
import analyze_b3
import analyze_micro
import b1_relay_diag
from common import spread, wilson


def test_wilson_matches_the_textbook_interval() -> None:
    w = wilson(5, 10)
    assert w["p"] == 0.5
    assert w["low"] == pytest.approx(0.2366, abs=1e-4)
    assert w["high"] == pytest.approx(0.7634, abs=1e-4)


def test_wilson_bounds_a_run_without_failures() -> None:
    # All 583 frames on time still allows a true miss rate of up to z^2 / (n + z^2).
    w = wilson(583, 583)
    assert w["high"] == pytest.approx(1.0)
    assert w["low"] == pytest.approx(583 / (583 + 1.959964**2))
    assert wilson(0, 0)["p"] is None


def test_spread_leaves_out_missing_values() -> None:
    assert spread([3.0, None, 1.0, 2.0, 10.0]) == {"n": 4, "median": 2.5, "min": 1.0, "max": 10.0}
    assert spread([]) == {"n": 0, "median": None, "min": None, "max": None}


def test_b1_model_of_independent_loss() -> None:
    assert analyze_b1.model_ratio("v2-nofec", 0.1, [2400]) == pytest.approx(0.9**2)
    # One parity packet per group of up to 4 repairs any single loss in the group.
    assert analyze_b1.model_ratio("v2-fec", 0.1, [2400]) == pytest.approx(0.9**3 + 3 * 0.1 * 0.9**2)
    # A NACKed packet is lost only when the original and all three retransmissions are.
    assert analyze_b1.model_ratio("v2-nack", 0.1, [1200]) == pytest.approx(1 - 0.1**4)
    # A 31,000-byte pickled frame (+24 bytes of pickle, +8 UDP) is 21 IP fragments.
    assert analyze_b1.model_ratio("legacy", 0.01, [31000]) == pytest.approx(0.99**21)


def _b1_run(rep: int, sent: int, on_time: int, audio_lost: int) -> dict[str, Any]:
    return {
        "tag": f"downlink-v2-fec-nack-50-r{rep}",
        "condition": "downlink",
        "system": "v2-fec-nack",
        "loss_percent": 5.0,
        "measured_udp_packet_loss": 0.05,
        "video": {
            "frames_sent": sent,
            "frames_on_time": on_time,
            "delivery_ratio": on_time / sent,
            "latency_ms": {"p50": 10.0 + rep, "p99": 40.0 + rep},
            "model_ratio_independent_loss": 0.99,
            "nack": {"nacks_sent": 10 * rep, "retransmissions": 5 * rep},
            "assembler": {"completed": on_time, "superseded": sent - on_time},
        },
        "audio": {"frames_sent": 1850, "frames_lost": audio_lost, "residual_loss": audio_lost / 1850},
        "receiver_frag_memory": None,
    }


def test_b1_pools_the_repeats_of_a_group() -> None:
    g = analyze_b1.aggregate([_b1_run(1, 583, 583, 0), _b1_run(2, 582, 580, 2)])
    assert g["runs"] == 2
    assert g["video"]["delivery_ratio"] == wilson(1163, 1165)
    assert g["video"]["delivery_ratio_per_run"] == spread([1.0, 580 / 582])
    assert g["video"]["nack_totals"] == {"nacks_sent": 30, "retransmissions": 15}
    assert g["video"]["assembler_totals"] == {"completed": 1163, "superseded": 2}
    assert g["audio"]["residual_loss"] == wilson(2, 3700)
    assert g["tags"] == ["downlink-v2-fec-nack-50-r1", "downlink-v2-fec-nack-50-r2"]


def test_relay_diag_needs_no_nack_when_fec_can_rebuild_the_frame() -> None:
    frame_of = dict.fromkeys(range(10, 14), 1) | dict.fromkeys(range(15, 19), 2)
    frame_of |= dict.fromkeys(range(20, 24), 3) | {30: 4, 31: 4, 40: 5}
    parity = [(14, 10, 4), (19, 15, 4), (24, 20, 4)]
    first_drop = dict.fromkeys(frame_of, False) | {14: False, 19: True, 24: False}
    first_drop[11] = True  # frame 1: one loss and its parity arrived, FEC rebuilds it
    first_drop[16] = True  # frame 2: one loss, but the parity packet was lost too
    first_drop[21] = first_drop[22] = True  # frame 3: two losses in one group
    first_drop[31] = True  # frame 4: a loss that no parity group covers (FEC off)
    need = b1_relay_diag.needs_nack(first_drop, frame_of, parity)
    assert need == {1: False, 2: True, 3: True, 4: True, 5: False}


def test_b3_point_needs_delivery_and_p99() -> None:
    assert analyze_b3.passes({"delivery_ratio": 0.99, "latency_ms": {"p99": 99.9}})
    assert not analyze_b3.passes({"delivery_ratio": 0.9899, "latency_ms": {"p99": 10.0}})
    assert not analyze_b3.passes({"delivery_ratio": 1.0, "latency_ms": {"p99": 100.0}})
    assert not analyze_b3.passes({"delivery_ratio": 1.0, "latency_ms": {}})  # nothing arrived


def _b3_point(n: int, rep: int, ok: bool) -> dict[str, Any]:
    return {
        "participants": n,
        "repeat": rep,
        "pass": ok,
        "delivery_ratio": 0.999 if ok else 0.98,
        "latency_ms": {"p99": 20.0},
        "server_cpu_utilisation": n / 320,
    }


def test_b3_capacity_is_the_largest_passing_size_of_each_repeat() -> None:
    sweep = [
        _b3_point(n, rep, ok)
        for rep, results in {
            1: {128: True, 144: True, 160: False, 176: False},
            2: {128: True, 144: False, 160: True, 176: False, 192: False},
            3: {128: True, 144: True, 160: True, 176: False, 192: False},
        }.items()
        for n, ok in results.items()
    ]
    s = analyze_b3.system_summary(sweep, [], repeats=3)
    assert s["capacity_by_repeat"] == [144, 160, 160]
    assert s["first_failing_by_repeat"] == [160, 144, 176]
    assert s["capacity_participants"] == spread([144, 160, 160])
    assert s["largest_size_every_repeat_passed"] == 128
    at_160 = next(x for x in s["sweep"] if x["participants"] == 160)
    assert (at_160["runs"], at_160["passes"]) == (3, 2)


def test_micro_parses_go_benchmark_lines() -> None:
    out = analyze_micro.parse(
        "pkg: example.com/x/server/internal/media\n"
        "BenchmarkForward/members=4-2   1000   1234 ns/op   56 B/op   0 allocs/op\n"
        "BenchmarkForward/members=4-2   1000   1250 ns/op   56 B/op   0 allocs/op\n"
        "PASS\n"
    )
    runs = out["media.BenchmarkForward/members=4"]
    assert [r["ns/op"] for r in runs] == [1234.0, 1250.0]
    assert runs[0]["allocs/op"] == 0.0
