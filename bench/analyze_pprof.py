"""Summarise a CPU profile of the v2 server (from bench/profile_b3.sh).

Usage: analyze_pprof.py PROFILE.pb.gz SERVER_BINARY [LOADGEN_RESULT.json [METRICS.txt]]
           [--provenance OUT.json --build BUILD.json --run-id ID --command CMD
            --server-args ARGS --raw-dir DIR]

Prints the provenance (run, command, server flags, build), the load that was applied,
the profile's own header (File, Build ID, Type, Time, Duration), the share of CPU time
spent in a few code paths (cumulative, so nested paths overlap), the server's system
call counters if a metrics scrape is given, and the top functions by own time. With
--provenance it also writes that information as JSON. The code-path patterns match
the server both before and after system-call batching. Everything comes from
`go tool pprof` and the server's own counters.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import ROOT, environment, parse_metrics, syscall_summary, write_json

# (label, regular expression matched against the function names of `pprof -top -cum`).
PATHS = [
    ("receive loop (read + route + forward)", r"^.*media\.\(\*Router\)\.Serve$"),
    # Before batching forwardVideo sent every copy itself; afterwards flush does.
    ("  forwarding video (sending the copies)", r"^.*media\.\(\*Router\)\.(forwardVideo|flush)$"),
    ("  reading packets", r"^(net\.\(\*UDPConn\)\.ReadFromUDPAddrPort|.*udpio\.\(\*Conn\)\.ReadBatch)$"),
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


def syscall_counters(counters: dict[str, Any]) -> None:
    """Datagrams per system call from a scrape of the server's /metrics."""
    print("\nserver counters for the whole run:")
    print(
        f"  batching {counters['udp_batching_enabled']}, GSO {counters['udp_gso_enabled']}"
        f" (the server's udp_batching_enabled and udp_gso_enabled gauges)"
    )
    reads, per_read = counters["read_syscalls"], counters["datagrams_per_read_call"]
    print(f"  datagrams accepted {counters['datagrams_received']:.0f}, read system calls {reads:.0f}", end="")
    print(f" ({per_read:.1f} datagrams per call)" if per_read else "")
    calls, per_call = counters["video_send_syscalls"], counters["video_datagrams_per_send_call"]
    sent = counters["video_datagrams_sent"]
    print(f"  video datagrams sent {sent:.0f}, send system calls {calls:.0f}", end="")
    print(f" ({per_call:.1f} per call)" if per_call else "")
    if counters["video_datagrams_per_message"]:
        msgs = counters["video_send_messages"]
        print(f"  messages {msgs:.0f} ({counters['video_datagrams_per_message']:.2f} datagrams per message)")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("profile")
    ap.add_argument("binary")
    ap.add_argument("loadgen", nargs="?", type=Path)
    ap.add_argument("metrics", nargs="?", type=Path)
    ap.add_argument("--provenance", type=Path, help="also write the provenance and counters here as JSON")
    ap.add_argument("--build", type=Path)
    ap.add_argument("--run-id", default="")
    ap.add_argument("--command", default="")
    ap.add_argument("--server-args", default="")
    ap.add_argument("--raw-dir", type=Path)
    args = ap.parse_args()
    if args.raw_dir:
        args.raw_dir = args.raw_dir.resolve()
    load: dict[str, Any] | None = None
    if args.run_id:
        print(f"run {args.run_id}: {args.command}")
        print(f"server flags: {args.server_args or '(defaults)'}")
    build = json.loads(args.build.read_text()) if args.build else None
    if build:
        dirty = ", ".join(build["git_dirty_files"]) or "none"
        print(f"built from {build['git_commit']} (uncommitted: {dirty}) at {build['utc']}")
        print(f"toolchain: {build.get('go', '')}")
        print("binaries (SHA-256): " + ", ".join(f"{k} {v[:16]}" for k, v in build["binaries"].items()))
    if args.loadgen and args.loadgen.exists():
        load = json.loads(args.loadgen.read_text())
        core = load.get("server_core") or {}
        nan = float("nan")
        print(
            f"load: {load['participants']} participants ({load['wire']}), "
            f"delivery {load['delivery_ratio']:.4f}, p99 {load['latency_ms'].get('p99', nan):.1f} ms, "
            f"server CPU {load.get('server_cpu_utilisation', nan):.3f} "
            f"(core {core.get('cpu')}: busy {core.get('utilisation', nan):.3f}, "
            f"softirq {core.get('softirq_seconds', nan):.2f} s of {core.get('busy_seconds', nan):.2f} s)"
        )
    cum = pprof(["-top", "-cum", "-nodecount=500", args.binary, args.profile])
    header = [
        line
        for line in cum.splitlines()
        if line.startswith(("File:", "Build ID:", "Type:", "Time:", "Duration", "Showing nodes"))
    ]
    print(*header, sep="\n")
    rows = [m.groups() for line in cum.splitlines() if (m := ROW.match(line))]
    print("\nshare of profiled CPU time by code path (cumulative):")
    shares = {}
    for label, pattern in PATHS:
        rx = re.compile(pattern)
        hits = [r for r in rows if rx.match(r[5])]
        shares[label.strip()] = share = max((float(r[4]) for r in hits), default=0.0)
        print(f"  {label:42s} {share:5.1f} %")
    counters = None
    if args.metrics and args.metrics.exists():
        counters = syscall_summary(parse_metrics(args.metrics.read_text()))
        syscall_counters(counters)
    flat = pprof(["-top", "-nodecount=20", args.binary, args.profile])
    print("\ntop functions by own time:")
    started = False
    for line in flat.splitlines():
        if line.lstrip().startswith("flat"):
            started = True
        if started:
            print(line)
    if args.provenance:
        write_json(
            args.provenance,
            {
                "benchmark": "CPU profile of the v2 server under the B3 load",
                "run_id": args.run_id,
                "command": args.command,
                "server_args": args.server_args,
                "raw_files": str(args.raw_dir.relative_to(ROOT)) if args.raw_dir else None,
                "pprof_header": header,
                "share_of_profile_percent": shares,
                "server_counters": counters,
                "load": load,
                "environment": environment(args.build),
            },
        )


if __name__ == "__main__":
    main()
