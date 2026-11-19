"""Combine the per-participant logs of bench/run_b2.sh into bench/results/b2_audio.json.

Playout latency is the time from the end of an audio frame's capture at the talker
(the tag's timestamp) to the moment the receiver's sound card starts playing it.
Video latency is measured from the sender's sendto() to the receiver storing (legacy)
or completing (v2) the frame. All processes share CLOCK_MONOTONIC.
"""

from __future__ import annotations

import bisect
import itertools
import json
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import RESULTS, environment, summary, write_json

STALL = 0.5  # a gap between two video updates from one sender longer than this is a stall


def _series(points: list[tuple[float, float]], t0: float, step: float = 0.5) -> list[list[float]]:
    """Mean of (t, value) points per `step` seconds, for plotting."""
    buckets: dict[int, list[float]] = defaultdict(list)
    for t, v in points:
        buckets[int((t - t0) / step)].append(v)
    return [[round(k * step, 2), round(sum(vs) / len(vs), 1)] for k, vs in sorted(buckets.items())]


def analyze_run(tag: str, d: Path) -> dict[str, Any]:
    meta = json.loads((d / f"{tag}.meta.json").read_text())
    system, n = meta["system"], meta["participants"]
    lo, hi = float(meta["window_start"]), float(meta["window_end"])
    seconds = hi - lo
    parts = [json.loads((d / f"{tag}.p{i}.json").read_text()) for i in range(1, n + 1)]
    key = "id" if system == "legacy" else "name"
    sends: dict[str, dict[int, list[float]]] = {}
    sent_counts: dict[str, int] = {}
    audio_sent: dict[int, set[int]] = {}
    for p in parts:
        per_index: dict[int, list[float]] = defaultdict(list)
        count = 0
        for e in p["sent_video"]:
            per_index[int(e[0])].append(float(e[1]))
            if lo <= float(e[1]) <= hi:
                count += 1
        for times in per_index.values():
            times.sort()
        sends[str(p[key])] = per_index
        sent_counts[str(p[key])] = count
        audio_sent[int(p["talker"])] = {int(s) for s, t in p["sent_audio"] if lo <= float(t) <= hi}

    receivers = []
    all_audio_latency: list[float] = []
    all_video_latency: list[float] = []
    for p in parts:
        me = str(p[key])
        played = [(int(e[0]), int(e[1]), float(e[2]), float(e[3])) for e in p["played"]]
        window = [x for x in played if lo <= x[2] <= hi]
        lat = [x[3] for x in window]
        all_audio_latency += lat
        heard = defaultdict(set)
        for talker, seq, _t, _l in played:
            heard[talker].add(seq)
        delivery = {
            str(t): len(heard[t] & seqs) / len(seqs) if seqs else None
            for t, seqs in audio_sent.items()
            if t != p["talker"]
        }
        first5 = [x[3] for x in window if x[2] < lo + 5]
        last5 = [x[3] for x in window if x[2] > hi - 5]

        video: dict[str, Any] = {}
        updates: dict[str, list[tuple[float, float]]] = defaultdict(list)
        for e in p["recv_video"]:
            sender, idx, t = str(e[0]), int(e[1]), float(e[2])
            if sender == me or not lo <= t <= hi:
                continue
            times = sends.get(sender, {}).get(idx)
            if not times:
                continue
            j = bisect.bisect_right(times, t) - 1
            if j >= 0:
                updates[sender].append((t, t - times[j]))
        vlat: list[float] = []
        worst_gap = 0.0
        stalled = 0.0
        frames = 0
        for sender in sends:
            if sender == me:
                continue
            ups = sorted(updates.get(sender, []))
            frames += len(ups)
            vlat += [x[1] for x in ups]
            stamps = [lo] + [x[0] for x in ups] + [hi]
            gaps = [b - a for a, b in itertools.pairwise(stamps)]
            worst_gap = max(worst_gap, *gaps)
            stalled += sum(g for g in gaps if g > STALL)
        expected = sum(c for s, c in sent_counts.items() if s != me)
        all_video_latency += vlat
        video = {
            "frames_expected": expected,
            "frames_received": frames,
            "delivery_ratio": frames / expected if expected else None,
            "latency_ms": summary(x * 1000 for x in vlat),
            "longest_gap_s": round(worst_gap, 3),
            "stalled_fraction": stalled / (seconds * max(1, len(parts) - 1)),
        }

        if system == "legacy":
            down = {}
            for kind, prefix in (("audio", "a"), ("video", "v")):
                c0, c1 = meta["nft_start"][f"{prefix}{p['id']}"], meta["nft_end"][f"{prefix}{p['id']}"]
                elapsed = float(meta["window_end"]) - float(meta["window_start"])
                down[kind] = {
                    "packets": c1[0] - c0[0],
                    "kbit_per_s": (c1[1] - c0[1]) * 8 / 1000 / elapsed,
                    "source": "nftables input counter (IP packet length) over the measurement window",
                }
        else:
            series = [(float(t), pk, by) for t, pk, by in p["rx_series"]]
            first = next(x for x in series if x[0] >= lo)
            last = [x for x in series if x[0] <= hi][-1]
            elapsed = last[0] - first[0]
            down = {}
            for kind in ("audio", "video"):
                packets = sum(last[1][k] - first[1][k] for k in (kind, f"{kind}_fec"))
                payload = sum(last[2][k] - first[2][k] for k in (kind, f"{kind}_fec"))
                media_packets = last[1][kind] - first[1][kind]
                media_payload = last[2][kind] - first[2][kind]
                down[kind] = {
                    "packets": packets,
                    "kbit_per_s": (payload + 28 * packets) * 8 / 1000 / elapsed,
                    "kbit_per_s_without_fec": (media_payload + 28 * media_packets) * 8 / 1000 / elapsed,
                    "source": "client receive counters + 28 B IPv4/UDP header per packet, over the window",
                }
        down_audio, down_video = down["audio"], down["video"]
        receivers.append(
            {
                "participant": me,
                "audio_latency_ms": summary(lat),
                "audio_latency_first_5s_ms": summary(first5)["mean"],
                "audio_latency_last_5s_ms": summary(last5)["mean"],
                "audio_delivery_by_talker": delivery,
                "audio_latency_series": _series([(x[2], x[3]) for x in window], lo),
                "video_latency_series": _series(
                    [(t, lat_s * 1000) for ups in updates.values() for t, lat_s in ups], lo
                ),
                "downstream_audio": down_audio,
                "downstream_video": down_video,
                "video": video,
                "chunks_played": p.get("chunks_played"),
            }
        )
    return {
        "system": system,
        "participants": n,
        "window_seconds": round(seconds, 1),
        "audio_latency_ms": summary(all_audio_latency),
        "video_latency_ms": summary(x * 1000 for x in all_video_latency),
        "downstream_audio_kbit_per_s_mean": sum(r["downstream_audio"]["kbit_per_s"] for r in receivers)
        / len(receivers),
        "receivers": receivers,
        "kernel": meta["snmp"],
    }


def main() -> None:
    d = Path(sys.argv[1])
    out = Path(sys.argv[2]) if len(sys.argv) > 2 else RESULTS / "b2_audio.json"
    runs = [analyze_run(m.name.removesuffix(".meta.json"), d) for m in sorted(d.glob("*.meta.json"))]
    runs.sort(key=lambda r: (r["system"] != "legacy", r["participants"]))
    write_json(
        out,
        {
            "benchmark": "B2 multi-talker audio",
            "command": "sudo bench/run_b2.sh",
            "setup": "everybody talks (tagged tone) and sends video (corpus, 15.625 fps); one network "
            "namespace, loopback; legacy speaker: fake PyAudio stream that accepts one 1024-sample chunk at "
            "a time and plays it in real time, so write() blocks for a chunk's duration when the receiver "
            "is behind",
            "stall_threshold_s": STALL,
            "runs": runs,
            "environment": environment(),
        },
    )
    print(f"wrote {out} ({len(runs)} runs)")


if __name__ == "__main__":
    main()
