"""Why did v2 lose frames in B1's both-rtt50 condition? A diagnosis from the relay's log.

Usage (after `RELAY_LOG=1 CONDITIONS=both-rtt50 ... bench/run_b1.sh`):

    python3 bench/b1_relay_diag.py bench/tmp/b1/<run ID> > bench/results/b1_rtt50_relay_diag.txt

For every both-rtt50 run with a relay log (TAG.relay.jsonl) it reads the downstream video
packets as they passed the relay (each then took the relay's one-way delay to the
receiver) and the receiver's NACKs upstream, and lists each frame the receiver never
completed: the fragments lost on their first transmission, when each retransmission
passed the relay (ms after the frame's first fragment) and whether it was dropped
again, whether every fragment got through in the end, and when the next frame's first
fragment passed. A frame whose repairs completed it only after the next frame's first
fragment had passed was usually superseded: the next frame needed no repair and completed first, and the
receiver's assembler is newest-wins. It also counts the retransmissions and how many of
them repeated one already sent (a NACK retry sent before the first repair could return).
FEC rebuilds at the receiver are not visible here.
"""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field
from pathlib import Path

PT_VIDEO = 96


@dataclass
class Frame:
    count: int
    first: float
    got: set[int] = field(default_factory=set)
    lost: list[int] = field(default_factory=list)
    repairs: list[tuple[float, int, bool]] = field(default_factory=list)


def diagnose(d: Path, tag: str) -> list[str]:
    log = [json.loads(line) for line in (d / f"{tag}.relay.jsonl").read_text().splitlines()]
    send = json.loads((d / f"{tag}.send.json").read_text())
    recv = json.loads((d / f"{tag}.recv.json").read_text())
    stats = json.loads((d / f"{tag}.relay.json").read_text())
    completed = {int(e[1]) for e in recv["recv_video"]}
    sent_order = [int(e[0]) for e in send["sent_video"]]
    frames: dict[int, Frame] = {}
    order: list[int] = []
    seen: dict[int, int] = {}
    rtx = duplicates = 0
    nacks = nacks_dropped = 0
    for t, direction, _length, dropped, head in log:
        b = bytes.fromhex(head)
        if direction == "up":
            if len(b) >= 2 and b[1] == 205:  # RTCP generic NACK
                nacks += 1
                nacks_dropped += int(dropped)
            continue
        if len(b) < 20 or b[1] & 0x7F != PT_VIDEO:
            continue
        seq = int.from_bytes(b[2:4], "big")
        fid = int.from_bytes(b[12:16], "big")
        idx = int.from_bytes(b[16:18], "big")
        f = frames.get(fid)
        if f is None:
            f = frames[fid] = Frame(int.from_bytes(b[18:20], "big"), t)
            order.append(fid)
        seen[seq] = seen.get(seq, 0) + 1
        if seen[seq] > 1:
            rtx += 1
            duplicates += int(seen[seq] > 2)
            f.repairs.append((t, idx, dropped))
        elif dropped:
            f.lost.append(idx)
        if not dropped:
            f.got.add(idx)
    down = stats["downstream"]
    out = [
        f"{tag}: relay one-way delay {stats['one_way_delay_ms']:g} ms, drop probability "
        f"{stats['loss_probability']:g}; downstream {down['dropped']}/{down['received']} dropped, upstream "
        f"{stats['upstream']['dropped']}/{stats['upstream']['received']} dropped",
        f"  NACKs {nacks} ({nacks_dropped} dropped); video retransmissions {rtx}, of which "
        f"{rtx - duplicates} first repairs and {duplicates} repeats of a packet already resent; "
        f"assembler {recv.get('assembler')}",
    ]
    # The k-th frame id seen downstream is the k-th frame sent (none of the first frames
    # was lost whole in these runs; checked by the fragment counts matching).
    for pos, idx in enumerate(sent_order):
        if idx in completed or pos >= len(order):
            continue
        f = frames[order[pos]]
        nxt = frames[order[pos + 1]].first if pos + 1 < len(order) else None
        repairs = ", ".join(
            f"{i}@+{1000 * (t - f.first):.0f}{' dropped' if dr else ''}" for t, i, dr in f.repairs
        )
        # When the frame had every fragment: each lost one's first repair that got through.
        first_ok = [
            min((t for t, i, dr in f.repairs if i == lost and not dr), default=None) for lost in f.lost
        ]
        verdict = (
            "all fragments got through"
            if len(f.got) == f.count
            else (
                f"fragments {sorted(set(range(f.count)) - f.got)} never got through "
                "(FEC may have rebuilt them)"
            )
        )
        timing = ""
        if nxt is not None and f.lost and all(t is not None for t in first_ok):
            done = max(t for t in first_ok if t is not None)
            timing = (
                f"; complete at the relay at +{1000 * (done - f.first):.0f} ms vs the next frame's first "
                f"fragment at +{1000 * (nxt - f.first):.0f} ms"
            )
        out.append(
            f"  frame {pos} ({f.count} fragments): lost {f.lost}; repairs [{repairs}]; {verdict}{timing}"
        )
    return out


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    d = Path(sys.argv[1])
    print(f"run {d.name}")
    for log in sorted(d.glob("both-rtt50-*.relay.jsonl")):
        print("\n".join(diagnose(d, log.name.removesuffix(".relay.jsonl"))))


if __name__ == "__main__":
    main()
