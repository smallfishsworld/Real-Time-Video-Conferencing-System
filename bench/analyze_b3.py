"""Summarise one run of bench/run_b3.sh into bench/results/b3_capacity*.json.

A point passes when its frame delivery is >= 99 % and its p99 frame latency < 100 ms.
For each system (a v2 variant or the legacy relay) the result has, per swept size,
how many repeats ran and passed and the median, minimum and maximum of each measure;
per repeat, the largest passing size (the sweep stops after two consecutive failures);
and the median, minimum and maximum of those capacities. The fixed-load runs give CPU
time per participant-minute. Only files of the run in RUNDIR are read, and a point
whose label names another run is an error.

Usage: analyze_b3.py RUNDIR OUT.json [--build BUILD.json]
       analyze_b3.py --check POINT.json      (one point's verdict, exit 0 pass / 3 fail)
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import environment, parse_metrics, spread, syscall_summary, write_json

DELIVERY, P99_MS = 0.99, 100.0
POINT = re.compile(r"^(?P<system>.+)-(?P<n>\d+)-(?P<label>sweep|fixed)-r(?P<rep>\d+)\.json$")


def passes(r: dict[str, Any]) -> bool:
    return bool(r["delivery_ratio"] >= DELIVERY and r["latency_ms"].get("p99", 1e9) < P99_MS)


def row(r: dict[str, Any], metrics: Path) -> dict[str, Any]:
    keys = (
        "participants",
        "delivery_ratio",
        "latency_ms",
        "audio_delivery_ratio",
        "server_cpu_seconds",
        "server_cpu_utilisation",
        "server_cpu_seconds_per_participant_minute",
        "server_core",
        "loadgen_cpu_utilisation",
        "frames_expected",
        "frames_received",
        "packets_sent",
        "send_errors",
        "incomplete_frames",
        "control_connections_lost",
        "window_seconds",
        "wire_traffic",
        "started_utc",
        "label",
    )
    out = {k: r.get(k) for k in keys} | {"pass": passes(r)}
    core = r.get("server_core")
    if core and core.get("busy_seconds"):
        # The part of the server core's busy time that the kernel accounted as softirq
        # (on loopback, mostly the receivers' IP receive path; see environment.kernel_config).
        out["server_core_softirq_share"] = core["softirq_seconds"] / core["busy_seconds"]
    if metrics.exists():
        out["server_counters"] = syscall_summary(parse_metrics(metrics.read_text()))
    return out


def check(path: Path) -> int:
    """Print one run's verdict for the sweep log; exit status 0 = pass, 3 = fail."""
    r = json.loads(path.read_text())
    lat = r["latency_ms"]
    core = r.get("server_core") or {}
    print(
        "PASS" if passes(r) else "FAIL",
        f"delivery={r['delivery_ratio']:.4f} p99={lat.get('p99', float('nan')):.1f}ms "
        f"server_cpu={r.get('server_cpu_utilisation', float('nan')):.2f} "
        f"core_softirq={core.get('softirq_seconds', float('nan')):.2f}s "
        f"loadgen_cpu={r['loadgen_cpu_utilisation']:.2f} control_lost={r.get('control_connections_lost')}",
        file=sys.stderr,
    )
    return 0 if passes(r) else 3


def per_size(points: list[dict[str, Any]]) -> dict[str, Any]:
    """Repeats of one system at one size."""

    def counter(name: str) -> list[float | None]:
        return [(p.get("server_counters") or {}).get(name) for p in points]

    return {
        "participants": points[0]["participants"],
        "runs": len(points),
        "passes": sum(p["pass"] for p in points),
        "delivery_ratio": spread(p["delivery_ratio"] for p in points),
        "p99_latency_ms": spread(p["latency_ms"].get("p99") for p in points),
        "server_cpu_utilisation": spread(p.get("server_cpu_utilisation") for p in points),
        "server_cpu_seconds_per_participant_minute": spread(
            p.get("server_cpu_seconds_per_participant_minute") for p in points
        ),
        "server_core_utilisation": spread((p.get("server_core") or {}).get("utilisation") for p in points),
        "server_core_softirq_share": spread(p.get("server_core_softirq_share") for p in points),
        "datagrams_per_read_call": spread(counter("datagrams_per_read_call")),
        "video_datagrams_per_send_call": spread(counter("video_datagrams_per_send_call")),
        "video_datagrams_per_message": spread(counter("video_datagrams_per_message")),
        "points": points,
    }


def system_summary(sweep: list[dict[str, Any]], fixed: list[dict[str, Any]], repeats: int) -> dict[str, Any]:
    by_size: dict[int, list[dict[str, Any]]] = defaultdict(list)
    for p in sweep:
        by_size[p["participants"]].append(p)
    sizes = [per_size(sorted(by_size[n], key=lambda p: p["repeat"])) for n in sorted(by_size)]
    capacity, first_fail = [], []
    for rep in range(1, repeats + 1):
        mine = sorted((p for p in sweep if p["repeat"] == rep), key=lambda p: p["participants"])
        if not mine:
            continue
        ok = [p["participants"] for p in mine if p["pass"]]
        capacity.append(max(ok) if ok else 0)
        first_fail.append(next((p["participants"] for p in mine if not p["pass"]), None))
    all_passed = [s["participants"] for s in sizes if s["passes"] == s["runs"] == repeats]
    gauges = {}
    for k in ("udp_batching_enabled", "udp_gso_enabled"):
        seen = {(p.get("server_counters") or {}).get(k) for p in sweep + fixed}
        gauges[k] = sorted(v for v in seen if v is not None)
    return {
        "capacity_by_repeat": capacity,
        "capacity_participants": spread(capacity),
        "first_failing_by_repeat": first_fail,
        # The largest size at which every repeat ran and passed.
        "largest_size_every_repeat_passed": max(all_passed, default=0),
        "server_gauges_seen": gauges,
        "sweep": sizes,
        "fixed": {
            "runs": len(fixed),
            "participants": fixed[0]["participants"] if fixed else None,
            "server_cpu_seconds_per_participant_minute": spread(
                p.get("server_cpu_seconds_per_participant_minute") for p in fixed
            ),
            "server_cpu_utilisation": spread(p.get("server_cpu_utilisation") for p in fixed),
            "server_core_softirq_share": spread(p.get("server_core_softirq_share") for p in fixed),
            "delivery_ratio": spread(p["delivery_ratio"] for p in fixed),
            "points": sorted(fixed, key=lambda p: p["repeat"]),
        },
    }


def main() -> None:
    if len(sys.argv) == 3 and sys.argv[1] == "--check":
        sys.exit(check(Path(sys.argv[2])))
    ap = argparse.ArgumentParser()
    ap.add_argument("rundir", type=Path)
    ap.add_argument("out", type=Path)
    ap.add_argument("--build", type=Path, help="the run's build record")
    args = ap.parse_args()
    args.rundir = args.rundir.resolve()
    settings = json.loads((args.rundir / "settings.json").read_text())
    run_id = settings["run_id"]
    sweeps: dict[str, list[dict[str, Any]]] = defaultdict(list)
    fixed: dict[str, list[dict[str, Any]]] = defaultdict(list)
    room_sizes, fps = set(), set()
    for f in sorted(args.rundir.glob("*.json")):
        m = POINT.match(f.name)
        if not m:
            continue  # settings.json, lo_gso_check.json
        r = json.loads(f.read_text())
        if not str(r.get("label", "")).startswith(f"{run_id}/"):
            sys.exit(f"{f}: label {r.get('label')!r} is not from run {run_id}")
        room_sizes.add(r["room_size"])
        fps.add(r["fps"])
        p = row(r, f.with_suffix(".metrics")) | {"repeat": int(m["rep"])}
        (fixed if m["label"] == "fixed" else sweeps)[m["system"]].append(p)
    variants = dict(v.split("=", 1) for v in settings["variants"].split())
    systems = {}
    for name in sorted(set(sweeps) | set(fixed), key=lambda s: (s == "legacy", s)):
        s = system_summary(sweeps[name], fixed[name], settings["repeats"])
        if name.startswith("v2-"):
            s = {"server_args": variants.get(name.removeprefix("v2-"), "").replace(",", " ")} | s
        systems[name] = s
    gso_check = json.loads((args.rundir / "lo_gso_check.json").read_text())
    write_json(
        args.out,
        {
            "benchmark": "B3 relay capacity",
            "run_id": run_id,
            "command": settings["command"],
            "raw_files": str(args.rundir.relative_to(Path(__file__).resolve().parents[1])),
            "criteria": {"delivery_ratio_min": DELIVERY, "p99_latency_ms_max": P99_MS},
            "setup": settings
            | {
                "room_size": sorted(room_sizes),
                "fps": sorted(fps),
                "lo_gso_check": gso_check,
                "note": "server pinned with taskset to one CPU; loadgen pinned to two others; points "
                "interleaved by repeat, size and system, each under the bench lock; v2 frame delivery counts "
                "only frames whose every fragment arrived (the load generator does not use FEC repair); "
                "server CPU is the process's utime+stime from /proc, which on loopback includes the softirq "
                "receive work of the packets it sends (kernel_config: no IRQ_TIME_ACCOUNTING)",
            },
            "systems": systems,
            "environment": environment(args.build),
        },
    )
    print(f"wrote {args.out}")
    for name, s in systems.items():
        print(
            name,
            "capacity by repeat",
            s["capacity_by_repeat"],
            "every repeat passed up to",
            s["largest_size_every_repeat_passed"],
            "fixed CPU-s/participant-min",
            s["fixed"]["server_cpu_seconds_per_participant_minute"],
        )


if __name__ == "__main__":
    main()
