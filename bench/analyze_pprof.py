"""Summarise a CPU profile of the v2 server (from bench/profile_b3.sh).

Usage: analyze_pprof.py PROFILE.pb.gz SERVER_BINARY [LOADGEN_RESULT.json [METRICS.txt]]

Prints the load that was applied, the profile's total samples, the share of CPU time
spent in a few code paths (cumulative, so nested paths overlap), the server's system
call counters if a metrics scrape is given, and the top functions by own time. The
code-path patterns match the server both before and after system-call batching.
Everything comes from `go tool pprof` and the server's own counters.
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


def syscall_counters(path: Path) -> None:
    """Datagrams per system call from a scrape of the server's /metrics."""
    v: dict[str, float] = {}
    for line in path.read_text().splitlines():
        if line and not line.startswith("#"):
            name, value = line.rsplit(" ", 1)
            v[name] = float(value)
    received = sum(x for k, x in v.items() if k.startswith("confserver_media_packets_received_total"))
    video = v.get('confserver_media_packets_sent_total{kind="video"}', 0.0)
    reads = v.get("confserver_media_read_syscalls_total", 0.0)
    calls = v.get("confserver_video_send_syscalls_total", 0.0)
    msgs = v.get("confserver_video_send_messages_total", 0.0)
    print("\nserver counters for the whole run:")
    print(f"  datagrams accepted {received:.0f}, read system calls {reads:.0f}", end="")
    print(f" ({received / reads:.1f} datagrams per call)" if reads else "")
    print(f"  video copies sent {video:.0f}, send system calls {calls:.0f}", end="")
    print(f" ({video / calls:.1f} per call)" if calls else "")
    if msgs:
        print(f"  messages {msgs:.0f} ({video / msgs:.2f} datagrams per message)")


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
    if len(sys.argv) > 4 and Path(sys.argv[4]).exists():
        syscall_counters(Path(sys.argv[4]))
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
