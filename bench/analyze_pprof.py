"""Summarise a CPU profile of the v2 server (from bench/profile_b3.sh).

Usage: analyze_pprof.py PROFILE.pb.gz SERVER_BINARY [LOADGEN_RESULT.json]

Prints the load that was applied, the profile's total samples, the share of CPU time
spent in a few code paths (cumulative, so nested paths overlap), and the top functions
by own time. Everything comes from `go tool pprof`.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from pathlib import Path

# (label, regular expression matched against the function names of `pprof -top -cum`).
PATHS = [
    ("receive loop (read + route + forward)", r"^.*media\.\(\*Router\)\.Serve$"),
    ("  forwarding video (sendto per copy)", r"^.*media\.\(\*Router\)\.forwardVideo$"),
    ("  reading packets (recvfrom)", r"^net\.\(\*UDPConn\)\.ReadFromUDPAddrPort$"),
    ("audio mixers (20 ms ticks)", r"^.*media\.\(\*mixRoom\)\.run$"),
    ("  mixed-stream sends", r"^.*media\.\(\*mixRoom\)\.write$"),
    (
        "all system calls (Syscall6)",
        r"^(internal/runtime/syscall|runtime/internal/syscall|syscall)\.Syscall6$",
    ),
    ("Go scheduler and netpoller", r"^runtime\.(schedule|findRunnable|netpoll)$"),
    ("garbage collector", r"^runtime\.gcBgMarkWorker$"),
]

ROW = re.compile(r"^\s*([\d.]+\w*)\s+([\d.]+)%\s+([\d.]+)%\s+([\d.]+\w*)\s+([\d.]+)%\s+(.+?)\s*$")


def pprof(args: list[str]) -> str:
    return subprocess.run(["go", "tool", "pprof", *args], capture_output=True, text=True, check=True).stdout


def main() -> None:
    profile, binary = sys.argv[1], sys.argv[2]
    if len(sys.argv) > 3 and Path(sys.argv[3]).exists():
        r = json.loads(Path(sys.argv[3]).read_text())
        print(
            f"load: {r['participants']} participants ({r['wire']}), delivery {r['delivery_ratio']:.4f}, "
            f"p99 {r['latency_ms'].get('p99', float('nan')):.1f} ms, "
            f"server CPU {r.get('server_cpu_utilisation', float('nan')):.2f}"
        )
    cum = pprof(["-top", "-cum", "-nodecount=500", binary, profile])
    header = [line for line in cum.splitlines() if line.startswith(("Duration", "Showing nodes"))]
    print(*header, sep="\n")
    rows = [m.groups() for line in cum.splitlines() if (m := ROW.match(line))]
    print("\nshare of profiled CPU time by code path (cumulative):")
    for label, pattern in PATHS:
        rx = re.compile(pattern)
        hits = [r for r in rows if rx.match(r[5])]
        share = max((float(r[4]) for r in hits), default=0.0)
        print(f"  {label:42s} {share:5.1f} %")
    flat = pprof(["-top", "-nodecount=20", binary, profile])
    print("\ntop functions by own time:")
    started = False
    for line in flat.splitlines():
        if line.lstrip().startswith("flat"):
            started = True
        if started:
            print(line)


if __name__ == "__main__":
    main()
