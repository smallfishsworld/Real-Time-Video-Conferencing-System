"""Combine the per-run logs of bench/run_b1.sh into bench/results/b1_loss.json.

Video: a frame counts as delivered when the receiver had the complete frame within
DEADLINE of the moment the sender handed it to the socket. Frames sent in the first
second and the last 1.5 s of a run are left out (start-up and shutdown edges).

Audio: residual loss is the fraction of the sender's audio frames (legacy: 64 ms
chunks; v2: 20 ms frames) whose tag never reached the speaker. Legacy has no
concealment, so that is also its audible gap fraction. For v2 the gap fraction after
concealment is the share of 20 ms speaker ticks inside the active window that played
silence (neither received audio nor PLC).
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import RESULTS, environment, load_frames, summary, write_json

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

    nft = net["nft"]
    measured = nft["udp_drop"] / nft["udp_in"] if nft.get("udp_in") else None
    ip = net["snmp_rcv"]["Ip"]
    udp = net["snmp_rcv"]["Udp"]
    p = net["loss_per_mille"] / 1000
    return {
        "system": system,
        "loss_percent": net["loss_per_mille"] / 10,
        "measured_udp_packet_loss": measured,
        "video": {
            "frames_sent": len(window),
            "frames_complete": len(arrivals),
            "frames_on_time": on_time,
            "delivery_ratio": on_time / len(window) if window else None,
            "complete_ratio": len(arrivals) / len(window) if window else None,
            "delivery_ratio_first_half": ratio([(i, t) for i, t in window if t < mid]),
            "delivery_ratio_second_half": ratio([(i, t) for i, t in window if t >= mid]),
            "model_ratio_independent_loss": model_ratio(system, p, sizes),
            "latency_ms": summary(x * 1000 for x in latencies),
            "sender_frames_rate_limited": send.get("video_frames_rate_limited", 0),
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
    }


def main() -> None:
    d = Path(sys.argv[1])
    out = Path(sys.argv[2]) if len(sys.argv) > 2 else RESULTS / "b1_loss.json"
    sizes = [len(f) for f in load_frames()]
    runs = []
    for net in sorted(d.glob("*.net.json")):
        tag = net.name.removesuffix(".net.json")
        runs.append(analyze_run(tag, d, sizes))
    order = {"legacy": 0, "v2-nofec": 1, "v2-fec": 2}
    runs.sort(key=lambda r: (order.get(r["system"], 9), r["loss_percent"]))
    write_json(
        out,
        {
            "benchmark": "B1 loss resilience",
            "command": "sudo bench/run_b1.sh",
            "deadline_ms": DEADLINE * 1000,
            "link": "veth pairs, MTU 1500; loss: nftables numgen random drop of UDP packets at the "
            "receiver's prerouting hook, priority -500 (before defragmentation), server -> receiver only",
            "media": "corpus frames 480x480 JPEG q85 at 15.625 fps (one per 1024-sample audio chunk, the "
            "legacy rate); 16 kHz mono PCM audio",
            "runs": runs,
            "environment": environment(),
        },
    )
    print(f"wrote {out} ({len(runs)} runs)")


if __name__ == "__main__":
    main()
