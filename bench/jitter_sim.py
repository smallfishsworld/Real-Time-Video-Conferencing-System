"""Jitter-buffer simulations in simulated time: clock drift, bursts and delay steps.

`client` runs every scenario of bench/jitter_scenarios.json through the client's
AudioJitterBuffer, the same way server/internal/media/jitter_sim_test.go runs them
through the server mixer's buffer: frame k is sent at k * period_us, arrives at the
scenario's arrival time, and the receiver pops one frame every 20 ms, 7 ms after the
send grid. Every packet that has arrived by a tick is pushed before that tick's pop.
It writes one JSON line per scenario.

`collect` merges the server's and the client's lines of one invocation directory into
bench/results/jitter_<label>.json. The simulation is deterministic, so it needs no
bench lock and the numbers do not depend on the machine.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

import numpy as np

from common import environment, write_json

BENCH = Path(__file__).resolve().parent
sys.path.insert(0, str(BENCH.parent / "client" / "src"))

from confclient.jitter import AudioJitterBuffer, PlayStatus
from confclient.protocol import AUDIO_SAMPLES_PER_FRAME

MASK64 = (1 << 64) - 1


def splitmix64(x: int) -> int:
    """The jitter source; jitter_sim_test.go uses the same function."""
    x = (x + 0x9E3779B97F4A7C15) & MASK64
    x = ((x ^ (x >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    x = ((x ^ (x >> 27)) * 0x94D049BB133111EB) & MASK64
    return x ^ (x >> 31)


def times(sc: dict[str, Any], seed: int) -> tuple[list[int], list[int]]:
    n = sc["frames"]
    send = [k * sc["period_us"] for k in range(n)]
    arrive = []
    for k in range(n):
        d = sc["delay_us"]
        if sc.get("step_from", 0) <= k < sc.get("step_to", 0):
            d += sc["step_us"]
        if sc.get("jitter_us", 0) > 0:
            d += splitmix64(seed + k) % sc["jitter_us"]
        arrive.append(send[k] + d)
    burst = min(sc.get("burst_frames", 0), n)
    for k in range(burst - 1):
        arrive[k] = arrive[burst - 1]
    return send, arrive


def summarize(ms: list[float]) -> dict[str, float]:
    if not ms:
        return {"median": -1, "p95": -1, "max": -1}
    s = sorted(ms)

    def rank(p: float) -> float:
        return s[int(p * (len(s) - 1) + 0.5)]

    return {"median": rank(0.5), "p95": rank(0.95), "max": s[-1]}


def frame_payload(k: int) -> bytes:
    pcm = np.zeros(AUDIO_SAMPLES_PER_FRAME, dtype=">i2")
    pcm[0] = k & 0x7FFF
    pcm[1] = k >> 15
    return pcm.tobytes()


def simulate(sc: dict[str, Any], seed: int, delay_frames: int) -> dict[str, Any]:
    send, arrive = times(sc, seed)
    order = sorted(range(sc["frames"]), key=lambda k: arrive[k])
    jb = AudioJitterBuffer(delay_frames)
    played_frame = [False] * sc["frames"]
    ticks: list[tuple[int, PlayStatus, float]] = []
    end = arrive[order[-1]] + 1_000_000
    ai = 0
    n = 0
    while True:
        t = 7000 + n * 20000
        if t > end:
            break
        while ai < len(order) and arrive[order[ai]] <= t:
            k = order[ai]
            jb.push((k * AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF, frame_payload(k))
            ai += 1
        frame, status = jb.pop()
        lat = 0.0
        if status is PlayStatus.PLAYED:
            assert frame is not None
            k = int(frame[0]) | int(frame[1]) << 15
            played_frame[k] = True
            lat = (t - send[k]) / 1000
        ticks.append((t, status, lat))
        n += 1

    res: dict[str, Any] = {
        "scenario": sc["name"],
        "impl": "client",
        "delay_frames": delay_frames,
        "frames": sc["frames"],
        "active_ticks": 0,
        "played": 0,
        "concealed": 0,
        "silent": 0,
        "late": jb.stats.late,
        "never_played": 0,
        "longest_unplayed_ms": 0,
        "gaps_over_60ms": 0,
    }
    first = next((i for i, (_, s, _) in enumerate(ticks) if s is PlayStatus.PLAYED), None)
    if first is None:
        return res
    last = send[-1]
    all_lat: list[float] = []
    tail: list[float] = []
    run = 0
    for t, status, lat in ticks[first:]:
        if t > last:
            break
        res["active_ticks"] += 1
        if status is PlayStatus.PLAYED:
            res["played"] += 1
            all_lat.append(lat)
            if t > last - 10_000_000:
                tail.append(lat)
            if run > 3:
                res["gaps_over_60ms"] += 1
            run = 0
            continue
        res["concealed" if status is PlayStatus.CONCEALED else "silent"] += 1
        # A gap in played frames: concealed and silent ticks both count, so the longest
        # gap is not the longest silence (see "silent" for ticks that played nothing).
        run += 1
        res["longest_unplayed_ms"] = max(res["longest_unplayed_ms"], run * 20)
    if run > 3:
        res["gaps_over_60ms"] += 1
    res["never_played"] = played_frame.count(False)
    res["latency_last_10s_ms"] = summarize(tail)
    res["latency_all_ms"] = summarize(all_lat)
    for key in ("skipped", "duplicates"):
        if hasattr(jb.stats, key):
            res[key] = getattr(jb.stats, key)
    return res


def cmd_client(args: argparse.Namespace) -> None:
    spec = json.loads(args.scenarios.read_text())
    with args.out.open("w") as f:
        for sc in spec["scenarios"]:
            f.write(json.dumps(simulate(sc, spec["seed"], args.delay_frames)) + "\n")


def cmd_collect(args: argparse.Namespace) -> None:
    lines: list[dict[str, Any]] = []
    for name in ("server.jsonl", "client.jsonl"):
        lines += [json.loads(x) for x in (args.rundir / name).read_text().splitlines() if x.strip()]
    env = environment()
    env["build"] = json.loads((args.rundir / "build.json").read_text())
    summary: dict[str, dict[str, Any]] = {}
    for x in lines:
        summary.setdefault(x["impl"], {})[x["scenario"]] = {
            "longest_unplayed_ms": x["longest_unplayed_ms"],
            "gaps_over_60ms": x["gaps_over_60ms"],
            "late": x["late"],
            "latency_last_10s_median_ms": x.get("latency_last_10s_ms", {}).get("median"),
        }
    write_json(
        args.out,
        {
            "benchmark": "jitter_simulation",
            "label": args.label,
            "description": (
                "Deterministic simulation of the server mixer's and the client's audio "
                "jitter buffers (bench/jitter_scenarios.json; server/internal/media/"
                "jitter_sim_test.go and bench/jitter_sim.py). Latency is pop time minus "
                "send time of each played frame; a gap is a run of ticks in which the "
                "sender was active but no received frame was played."
            ),
            "command": args.command,
            "rundir": str(args.rundir.resolve().relative_to(BENCH.parent)),
            "environment": env,
            "scenarios": json.loads((args.rundir / "scenarios.json").read_text()),
            "summary": summary,
            "results": lines,
        },
    )


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("client", help="simulate the client's AudioJitterBuffer")
    c.add_argument("--scenarios", type=Path, default=BENCH / "jitter_scenarios.json")
    c.add_argument("--delay-frames", type=int, default=3, help="the client's default (--jitter-ms 60)")
    c.add_argument("--out", type=Path, required=True)
    c.set_defaults(fn=cmd_client)
    m = sub.add_parser("collect", help="merge one invocation's lines into a result file")
    m.add_argument("--rundir", type=Path, required=True)
    m.add_argument("--label", required=True)
    m.add_argument("--command", required=True)
    m.add_argument("--out", type=Path, required=True)
    m.set_defaults(fn=cmd_collect)
    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
