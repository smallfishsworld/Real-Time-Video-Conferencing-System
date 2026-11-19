"""Turn `go test -bench -benchmem` output into bench/results/micro.json (median of runs)."""

from __future__ import annotations

import re
import statistics
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import environment, write_json

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
    raw, out = Path(sys.argv[1]), Path(sys.argv[2])
    runs = parse(raw.read_text())
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
        out,
        {
            "benchmark": "Go microbenchmarks",
            "command": "bench/run_micro.sh",
            "note": "go test -bench . -benchmem -count 5 -cpu 1, pinned with taskset to one CPU",
            "raw_output": str(raw.name),
            "benchmarks": rows,
            "environment": environment(),
        },
    )
    for r in rows:
        print(f"{r['name']:55s} {r['median'].get('ns/op', 0):10.1f} ns/op")


if __name__ == "__main__":
    main()
