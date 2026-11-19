"""Turn `go test -bench -benchmem` output into bench/results/micro.json (median of runs)."""

from __future__ import annotations

import argparse
import re
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import ROOT, environment, write_json

LINE = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(.*)$")


def parse(text: str) -> dict[str, list[dict[str, float]]]:
    runs: dict[str, list[dict[str, float]]] = {}
    pkg = ""
    for line in text.splitlines():
        if line.startswith("pkg: "):
            pkg = line.split("/")[-1]
            continue
        m = LINE.match(line)
        if not m:
            continue
        name, iters, rest = m.groups()
        values = {"iterations": float(iters)}
        fields = rest.split()
        for value, unit in zip(fields[::2], fields[1::2], strict=False):
            values[unit] = float(value)
        runs.setdefault(f"{pkg}.{name}", []).append(values)
    return runs


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("raw", type=Path)
    ap.add_argument("out", type=Path)
    ap.add_argument("--build", type=Path, help="build record of the test binaries")
    ap.add_argument("--command", default="bench/run_micro.sh")
    ap.add_argument("--count", type=int, default=5)
    ap.add_argument("--cpu", default="1")
    args = ap.parse_args()
    runs = parse(args.raw.read_text())
    rows: list[dict[str, Any]] = []
    for name, rs in runs.items():
        units = sorted({u for r in rs for u in r} - {"iterations"})
        rows.append(
            {
                "name": name,
                "runs": len(rs),
                "median": {u: statistics.median(r[u] for r in rs if u in r) for u in units},
                "min": {u: min(r[u] for r in rs if u in r) for u in units},
                "max": {u: max(r[u] for r in rs if u in r) for u in units},
            }
        )
    write_json(
        args.out,
        {
            "benchmark": "Go microbenchmarks",
            "command": args.command,
            "note": f"go test -bench . -benchmem -count {args.count} -cpu 1, pinned with taskset to CPU "
            f"{args.cpu}. Send benchmarks (udpio.BenchmarkSendFrame, media.BenchmarkForwardVideo) use "
            "loopback receivers: their times include the receive path, which on lo runs in softirq on the "
            "sending CPU, and lo passes a UDP GSO message up that path whole, so GSO speed-ups there are "
            "loopback-only upper bounds.",
            "raw_output": str(args.raw.relative_to(ROOT)) if args.raw.is_relative_to(ROOT) else str(args.raw),
            "benchmarks": rows,
            "environment": environment(args.build),
        },
    )
    for r in rows:
        print(f"{r['name']:55s} {r['median'].get('ns/op', 0):10.1f} ns/op")


if __name__ == "__main__":
    main()
