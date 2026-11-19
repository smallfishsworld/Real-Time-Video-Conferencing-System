"""Summarise the load generator results of bench/run_b3.sh into
bench/results/b3_capacity*.json.

Capacity is the largest swept participant count whose run had frame delivery >= 99 %
and p99 frame latency < 100 ms (the sweep stops after two consecutive failures).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import RESULTS, environment, write_json

DELIVERY, P99_MS = 0.99, 100.0


def passes(r: dict[str, Any]) -> bool:
    return bool(r["delivery_ratio"] >= DELIVERY and r["latency_ms"].get("p99", 1e9) < P99_MS)


def row(r: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "participants",
        "offered_upstream_mbps",
        "frames_expected",
        "frames_received",
        "delivery_ratio",
        "latency_ms",
        "audio_delivery_ratio",
        "server_cpu_seconds",
        "server_cpu_utilisation",
        "server_cpu_seconds_per_participant_minute",
        "loadgen_cpu_utilisation",
        "packets_sent",
        "send_errors",
        "incomplete_frames",
        "control_connections_lost",
        "window_seconds",
    )
    return {k: r.get(k) for k in keys} | {"pass": passes(r)}


def check(path: Path) -> int:
    """Print one run's verdict for the sweep log; exit status 0 = pass, 3 = fail."""
    r = json.loads(path.read_text())
    lat = r["latency_ms"]
    print(
        "PASS" if passes(r) else "FAIL",
        f"delivery={r['delivery_ratio']:.4f} p99={lat.get('p99', float('nan')):.1f}ms "
        f"server_cpu={r.get('server_cpu_utilisation', float('nan')):.2f} "
        f"loadgen_cpu={r['loadgen_cpu_utilisation']:.2f} control_lost={r.get('control_connections_lost')}",
        file=sys.stderr,
    )
    return 0 if passes(r) else 3


def main() -> None:
    if len(sys.argv) == 3 and sys.argv[1] == "--check":
        sys.exit(check(Path(sys.argv[2])))
    ap = argparse.ArgumentParser()
    ap.add_argument("dir", type=Path)
    ap.add_argument("out", type=Path, nargs="?", default=RESULTS / "b3_capacity.json")
    ap.add_argument("--mtu", type=int, default=65536)
    ap.add_argument("--server-cpu", default="1")
    ap.add_argument("--loadgen-cpus", default="2,3")
    ap.add_argument("--server-args", default="", help="extra flags the v2 server ran with")
    ap.add_argument("--command", default="sudo bench/run_b3.sh", help="the command line as run")
    args = ap.parse_args()
    systems: dict[str, Any] = {}
    for f in sorted(args.dir.glob("*.json")):
        r = json.loads(f.read_text())
        s = systems.setdefault(r["wire"], {"sweep": [], "fixed": None})
        if r.get("label") == "fixed":
            s["fixed"] = row(r)
        else:
            s["sweep"].append(row(r))
    for s in systems.values():
        s["sweep"].sort(key=lambda x: x["participants"])
        ok = [x["participants"] for x in s["sweep"] if x["pass"]]
        s["capacity_participants"] = max(ok) if ok else 0
        first_fail = next((x["participants"] for x in s["sweep"] if not x["pass"]), None)
        s["first_failing_participants"] = first_fail
    write_json(
        args.out,
        {
            "benchmark": "B3 relay capacity",
            "command": args.command,
            "criteria": {"delivery_ratio_min": DELIVERY, "p99_latency_ms_max": P99_MS},
            "setup": {
                "loopback_mtu": args.mtu,
                "room_size": 4,
                "fps": 15,
                "server_cpu": args.server_cpu,
                "loadgen_cpus": args.loadgen_cpus,
                "v2_server_args": args.server_args,
                "note": "server pinned with taskset to one CPU; loadgen pinned to two others; v2 frame "
                "delivery counts only frames whose every fragment arrived (FEC repair not used by the "
                "load generator)",
            },
            "systems": systems,
            "environment": environment(),
        },
    )
    print(f"wrote {args.out}")
    for wire, s in systems.items():
        print(wire, "capacity", s["capacity_participants"], "fixed", s["fixed"])


if __name__ == "__main__":
    main()
