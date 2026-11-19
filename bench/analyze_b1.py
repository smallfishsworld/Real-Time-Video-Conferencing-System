"""Combine one run of bench/run_b1.sh into bench/results/b1_loss.json.

Video: a frame counts as delivered when the receiver had the complete frame within
DEADLINE of the moment the sender handed it to the socket. Frames sent in the first
second and the last 1.5 s of a run are left out (start-up and shutdown edges).

Audio: residual loss is the fraction of the sender's audio frames (legacy: 64 ms
chunks; v2: 20 ms frames) whose tag never reached the speaker. Legacy has no
concealment, so that is also its audible gap fraction. For v2 the gap fraction after
concealment is the share of 20 ms speaker ticks inside the active window that played
silence (neither received audio nor PLC).

Each (condition, system, loss rate) group pools its repeats: frames on time out of
frames sent, with a Wilson 95 % score interval, and the median, minimum and maximum
of the per-run ratios. The interval treats frames as independent trials; where
losses are correlated in time (the original's reassembly queue), the run-to-run range
is the better guide. Only the files of the run in RUNDIR are read; a run whose
processes failed has no .net.json and is listed under failed_runs.

Usage: analyze_b1.py RUNDIR OUT.json [--build BUILD.json] [--command CMD]
"""

from __future__ import annotations

import argparse
import json
import math
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import ROOT, environment, load_frames, spread, summary, wilson, write_json

DEADLINE = 0.2  # seconds from send to complete frame
EDGE_START, EDGE_END = 1.0, 1.5


def _ip_fragments(pickled_len: int, mtu: int = 1500) -> int:
    payload = pickled_len + 8  # UDP header
    per_fragment = (mtu - 20) // 8 * 8
    return math.ceil(payload / per_fragment)


def model_ratio(system: str, p: float, sizes: list[int]) -> float:
    """Expected frame delivery if every packet were lost independently with probability p."""
    if system == "legacy":
        n = [_ip_fragments(s + 24) for s in sizes]  # pickle protocol 4 adds 24 bytes
        return sum((1 - p) ** k for k in n) / len(n)
    frags = [math.ceil(s / 1200) for s in sizes]
    if system == "v2-nofec":
        return sum((1 - p) ** k for k in frags) / len(frags)
    if system.endswith("nack"):
        # Each packet gets the original try and up to three NACKed retransmissions, each
        # lost independently (the NACKs themselves travel the unimpaired direction).
        # FEC can only help further, so this is a lower bound for v2-fec-nack.
        q = 1 - p**4
        return sum(q**k for k in frags) / len(frags)
    total = 0.0
    for f in frags:  # groups of 4 media packets + 1 parity, never spanning frames
        prob = 1.0
        left = f
        while left > 0:
            k = min(4, left)
            m = k + 1
            prob *= (1 - p) ** m + m * p * (1 - p) ** (m - 1)
            left -= k
        total += prob
    return total / len(frags)


def analyze_run(tag: str, d: Path, sizes: list[int]) -> dict[str, Any]:
    send = json.loads((d / f"{tag}.send.json").read_text())
    recv = json.loads((d / f"{tag}.recv.json").read_text())
    net = json.loads((d / f"{tag}.net.json").read_text())
    system = net["system"]

    sent = [(int(e[0]), float(e[1])) for e in send["sent_video"]]
    t_first, t_last = sent[0][1], sent[-1][1]
    lo, hi = t_first + EDGE_START, t_last - EDGE_END
    window = [(i, t) for i, t in sent if lo <= t <= hi]
    send_time = dict(window)
    arrivals: dict[int, float] = {}
    for e in recv["recv_video"]:
        idx, t = int(e[1]), float(e[2])
        if idx in send_time and idx not in arrivals:
            arrivals[idx] = t
    latencies = [arrivals[i] - send_time[i] for i in arrivals]
    on_time = sum(1 for x in latencies if x <= DEADLINE)
    mid = (lo + hi) / 2

    def ratio(part: list[tuple[int, float]]) -> float | None:
        ok = sum(1 for i, t in part if i in arrivals and arrivals[i] - t <= DEADLINE)
        return ok / len(part) if part else None

    audio_sent = {int(s): float(t) for s, t in send["sent_audio"] if lo <= float(t) <= hi}
    audio_got = {int(e[1]) for e in recv["played"] if int(e[1]) in audio_sent}
    audio: dict[str, Any] = {
        "frames_sent": len(audio_sent),
        "frames_lost": len(audio_sent) - len(audio_got),
        "frame_ms": 64 if system == "legacy" else 20,
        "residual_loss": 1 - len(audio_got) / len(audio_sent) if audio_sent else None,
    }
    if system == "legacy":
        audio["gap_fraction_after_concealment"] = audio["residual_loss"]
    else:
        ticks = [s for t, s in recv["ticks"] if lo + 0.2 <= float(t) <= hi]
        audio["ticks"] = len(ticks)
        audio["concealed_fraction"] = ticks.count("concealed") / len(ticks) if ticks else None
        audio["gap_fraction_after_concealment"] = ticks.count("silent") / len(ticks) if ticks else None
        audio["fec_recovered_packets"] = recv.get("fec_recovered")

    frag: dict[str, Any] = {}
    frag_file = d / f"{tag}.frag.txt"
    if frag_file.exists():
        samples = [tuple(map(float, line.split())) for line in frag_file.read_text().splitlines() if line]
        high = net.get("ipfrag_high_thresh")
        frag = {
            "samples": len(samples),
            "max_bytes": int(max((b for _, _, b in samples), default=0)),
            "max_queues": int(max((q for _, q, _ in samples), default=0)),
            "ipfrag_high_thresh": high,
            "ipfrag_time_s": net.get("ipfrag_time"),
            # Seconds from the first video frame until the memory first reached 90 % of
            # the high threshold (None if it never did).
            "first_near_limit_s": next(
                (round(t - t_first, 1) for t, _, b in samples if high and b >= 0.9 * high), None
            ),
            "series_s_mib": [
                [round(t - t_first, 1), round(b / 2**20, 3)] for t, _, b in samples if t >= t_first
            ][::2],
        }

    # Delivery in 30 s bins of send time, for the dynamics of long runs.
    bins: dict[int, list[int]] = defaultdict(lambda: [0, 0])
    for i, t in window:
        b = bins[int((t - lo) // 30)]
        b[0] += 1
        b[1] += int(i in arrivals and arrivals[i] - t <= DEADLINE)
    by_30s = [{"from_s": 30 * k, "frames": n, "on_time": ok} for k, (n, ok) in sorted(bins.items())]

    nft = net["nft"]
    measured = nft["udp_drop"] / nft["udp_in"] if nft.get("udp_in") else None
    relay = None
    relay_file = d / f"{tag}.relay.json"
    if relay_file.exists():
        relay = json.loads(relay_file.read_text())
        up, down = relay["upstream"], relay["downstream"]
        relay["measured_loss_upstream"] = up["dropped"] / up["received"] if up["received"] else None
        relay["measured_loss_downstream"] = down["dropped"] / down["received"] if down["received"] else None
        measured = relay["measured_loss_downstream"]
    ip = net["snmp_rcv"]["Ip"]
    udp = net["snmp_rcv"]["Udp"]
    p = net["loss_per_mille"] / 1000
    condition = net.get("condition", "downlink")
    # The model assumes independent losses on the downlink only.
    model = model_ratio(system, p, sizes) if condition != "both-rtt50" else None
    return {
        "tag": tag,
        "condition": condition,
        "repeat": net.get("repeat"),
        "seconds": net.get("seconds"),
        "system": system,
        "loss_percent": net["loss_per_mille"] / 10,
        "measured_udp_packet_loss": measured,
        "relay": relay,
        "video": {
            "frames_sent": len(window),
            "frames_complete": len(arrivals),
            "frames_on_time": on_time,
            "delivery_ratio": on_time / len(window) if window else None,
            "complete_ratio": len(arrivals) / len(window) if window else None,
            "delivery_ratio_first_half": ratio([(i, t) for i, t in window if t < mid]),
            "delivery_ratio_second_half": ratio([(i, t) for i, t in window if t >= mid]),
            "model_ratio_independent_loss": model,
            "delivery_by_30s": by_30s,
            "latency_ms": summary(x * 1000 for x in latencies),
            "sender_frames_rate_limited": send.get("video_frames_rate_limited", 0),
            "nack": recv.get("nack") or None,
            # v2 only: frames the receiver abandoned incomplete, past the 300 ms
            # deadline (expired) or because a newer frame completed first (superseded).
            "assembler": recv.get("assembler") or None,
        },
        "audio": audio,
        "receiver_kernel": {
            "ip_reasm_reqds": ip["ReasmReqds"],
            "ip_reasm_oks": ip["ReasmOKs"],
            "ip_reasm_fails": ip["ReasmFails"],
            "ip_reasm_timeout": ip.get("ReasmTimeout", 0),
            "udp_in_datagrams": udp["InDatagrams"],
            "udp_rcvbuf_errors": udp["RcvbufErrors"],
        },
        "receiver_frag_memory": frag,
    }


CONDITIONS = {
    "downlink": "best case for recovery: nftables drops UDP packets at the receiver's prerouting hook, "
    "priority -500 (before defragmentation, so single IP fragments), server -> receiver only; NACKs and "
    "REGISTERs take an unimpaired path with a sub-millisecond round trip",
    "both-rtt50": "userspace relay (bench/lib/udp_impair.py) between the receiver and the server: "
    "independent drops per datagram in both directions and a fixed one-way delay each way (25 ms by "
    "default: +50 ms round trip); NACKs, retransmissions, parity and REGISTERs can be lost; v2 only "
    "(the relay works on whole datagrams)",
    "downlink-long": "the downlink condition for the original only, in long runs, for its dynamics as "
    "the receiver's IP reassembly queue fills",
}


def aggregate(runs: list[dict[str, Any]]) -> dict[str, Any]:
    """Pool the repeats of one condition, system and loss rate."""
    sent = sum(r["video"]["frames_sent"] for r in runs)
    on_time = sum(r["video"]["frames_on_time"] for r in runs)
    audio_sent = sum(r["audio"]["frames_sent"] for r in runs)
    audio_lost = sum(r["audio"]["frames_lost"] for r in runs)
    nacks = [r["video"]["nack"] for r in runs if r["video"]["nack"]]
    assembled = [r["video"]["assembler"] for r in runs if r["video"].get("assembler")]
    return {
        "condition": runs[0]["condition"],
        "system": runs[0]["system"],
        "loss_percent": runs[0]["loss_percent"],
        "runs": len(runs),
        "measured_udp_packet_loss": spread(r["measured_udp_packet_loss"] for r in runs),
        "video": {
            "frames_sent": sent,
            "frames_on_time": on_time,
            "delivery_ratio": wilson(on_time, sent),
            "delivery_ratio_per_run": spread(r["video"]["delivery_ratio"] for r in runs),
            "latency_ms_p50_per_run": spread(r["video"]["latency_ms"]["p50"] for r in runs),
            "latency_ms_p99_per_run": spread(r["video"]["latency_ms"]["p99"] for r in runs),
            "model_ratio_independent_loss": runs[0]["video"]["model_ratio_independent_loss"],
            "nack_totals": {k: sum(n.get(k, 0) for n in nacks) for k in nacks[0]} if nacks else None,
            "assembler_totals": (
                {k: sum(a.get(k, 0) for a in assembled) for k in assembled[0]} if assembled else None
            ),
        },
        "audio": {
            "frames_sent": audio_sent,
            "residual_loss": wilson(audio_lost, audio_sent),
            "residual_loss_per_run": spread(r["audio"]["residual_loss"] for r in runs),
            "gap_fraction_after_concealment_per_run": spread(
                r["audio"].get("gap_fraction_after_concealment") for r in runs
            ),
        },
        "receiver_frag_memory_max_bytes": spread(
            (r["receiver_frag_memory"] or {}).get("max_bytes") for r in runs
        ),
        "tags": [r["tag"] for r in runs],
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("rundir", type=Path)
    ap.add_argument("out", type=Path)
    ap.add_argument("--build", type=Path, help="the run's build record")
    ap.add_argument("--command", default="sudo bench/run_b1.sh")
    args = ap.parse_args()
    d = args.rundir
    sizes = [len(f) for f in load_frames()]
    run_id = d.name
    runs = []
    for net in sorted(d.glob("*.net.json")):
        meta = json.loads(net.read_text())
        if meta.get("run_id") != run_id:
            sys.exit(f"{net}: from run {meta.get('run_id')!r}, not {run_id}")
        runs.append(analyze_run(net.name.removesuffix(".net.json"), d, sizes))
    failed = sorted(
        f.name.removesuffix(".status") for f in d.glob("*.status") if f.read_text().strip() != "0"
    )
    order = {"legacy": 0, "v2-nofec": 1, "v2-fec": 2, "v2-nack": 3, "v2-fec-nack": 4}
    cond_order = {c: i for i, c in enumerate(CONDITIONS)}

    def key(r: dict[str, Any]) -> tuple[int, int, float]:
        return cond_order.get(r["condition"], 9), order.get(r["system"], 9), r["loss_percent"]

    runs.sort(key=lambda r: (*key(r), r["repeat"] or 0))
    groups: dict[tuple[Any, ...], list[dict[str, Any]]] = defaultdict(list)
    for r in runs:
        groups[key(r)].append(r)
    write_json(
        args.out,
        {
            "benchmark": "B1 loss resilience",
            "run_id": run_id,
            "command": args.command,
            "raw_files": str(d.relative_to(ROOT)) if d.is_relative_to(ROOT) else str(d),
            "deadline_ms": DEADLINE * 1000,
            "link": "veth pairs, MTU 1500, the server's veth towards the receiver with gso_max_segs 1",
            "conditions": {c: v for c, v in CONDITIONS.items() if any(r["condition"] == c for r in runs)},
            "media": "corpus frames 480x480 JPEG q85 at 15.625 fps (one per 1024-sample audio chunk, the "
            "legacy rate); 16 kHz mono PCM audio",
            "groups": [aggregate(g) for _, g in sorted(groups.items())],
            "failed_runs": failed,
            "runs": runs,
            "environment": environment(args.build),
        },
    )
    print(f"wrote {args.out} ({len(runs)} runs, {len(failed)} failed)")
    for _, g in sorted(groups.items()):
        a = aggregate(g)
        v, au = a["video"]["delivery_ratio"], a["audio"]["residual_loss"]
        print(
            f"{a['condition']:13s} {a['system']:12s} {a['loss_percent']:5.1f} % x{a['runs']}: "
            f"video {v['p']:.4f} [{v['low']:.4f}, {v['high']:.4f}]  audio lost {au['p']:.4f}"
        )


if __name__ == "__main__":
    main()
