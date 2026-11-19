"""Collect the JSON lines of bench/run_churn.sh into one result file.

Each run of server/internal/media TestChurnForwarding appends one JSON line per
configuration to run<N>.jsonl in the invocation's own directory. This script reads
only that directory, checks that every run covered the same configurations, and writes
the raw lines plus, per configuration, the median and the min-max across runs of the
delivered count and of each latency percentile.
"""

from __future__ import annotations

import argparse
import json
import statistics
from pathlib import Path
from typing import Any

from common import environment, write_json


def spread(values: list[float]) -> dict[str, float]:
    return {"median": statistics.median(values), "min": min(values), "max": max(values)}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--rundir", type=Path, required=True)
    ap.add_argument("--label", required=True)
    ap.add_argument("--command", required=True)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    files = sorted(args.rundir.glob("run*.jsonl"), key=lambda p: int(p.stem[3:]))
    runs: list[list[dict[str, Any]]] = []
    for f in files:
        runs.append([json.loads(line) for line in f.read_text().splitlines() if line.strip()])
    if not runs:
        raise SystemExit(f"no run*.jsonl in {args.rundir}")
    configs = [r["config"] for r in runs[0]]
    for i, r in enumerate(runs):
        if [x["config"] for x in r] != configs:
            raise SystemExit(f"run {i + 1} has configurations {[x['config'] for x in r]}, not {configs}")

    summary: dict[str, Any] = {}
    for c in configs:
        rows = [next(x for x in r if x["config"] == c) for r in runs]
        summary[c] = {
            "runs": len(rows),
            "sent_per_run": rows[0]["sent"],
            "delivered": spread([float(x["delivered"]) for x in rows]),
            "latency_ms": {
                q: spread([float(x["latency_ms"][q]) for x in rows]) for q in ("p50", "p90", "p99", "max")
            },
        }

    env = environment()
    env["build"] = json.loads((args.rundir / "build.json").read_text())
    write_json(
        args.out,
        {
            "benchmark": "churn",
            "label": args.label,
            "description": (
                "Forwarding latency of a 50 packet/s video stream between two members of one "
                "room while 1024 members send keepalives and other rooms churn the control "
                "plane (server/internal/media/churn_test.go)."
            ),
            "command": args.command,
            "rundir": str(args.rundir.relative_to(Path(__file__).resolve().parent.parent)),
            "environment": env,
            "summary": summary,
            "runs": runs,
        },
    )


if __name__ == "__main__":
    main()
