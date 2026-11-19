"""Which frames did B1's NACK runs lose, and what happened to the frame after each?

Usage (from the repository root, after bench/run_b1.sh):

    python3 bench/b1_lost_frames.py bench/tmp/b1/<run ID> > bench/results/b1_lost_frames.txt

For every lossy run of a system with NACK in that run directory it lists the frames of
the counted window (the same window as bench/analyze_b1.py) that never arrived complete,
and for each the frame that followed it: whether it arrived, its latency (send to
complete) and its assembly time (first fragment to complete). A next frame that was
complete within about a millisecond of its first fragment needed no retransmission, so
it finished before any retransmission for the frame before it could arrive; the
receiver's newest-wins assembler then discards the older frame (the receiver's
"superseded" counter, printed per run with "expired": frames still incomplete 300 ms
after their first fragment). A summary per condition, system and loss rate follows,
counting the next frames complete within QUICK_MS of their first fragment, too soon for
any retransmission (the client waits 5 ms before it asks).

Frames are identified by their corpus index, so a run must be shorter than the corpus
(1000 frames, 64 s); a run that sends an index twice is reported and skipped.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

EDGE_START, EDGE_END = 1.0, 1.5  # as in bench/analyze_b1.py
QUICK_MS = 1.5


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    raw = Path(sys.argv[1])
    print(f"run {raw.name}")
    print(
        "condition system loss repeat counted lost [superseded expired] | "
        "per lost frame: next frame latency ms / assembly ms"
    )
    # (condition, system, loss %) -> [lost, next lost, next quick, next slower]
    totals: dict[tuple[str, str, float], list[int]] = {}
    for net in sorted(raw.glob("*nack-*.net.json")):
        meta = json.loads(net.read_text())
        if meta["loss_per_mille"] == 0:
            continue
        tag = net.name.removesuffix(".net.json")
        send = json.loads((raw / f"{tag}.send.json").read_text())
        recv = json.loads((raw / f"{tag}.recv.json").read_text())
        sent = [(int(e[0]), float(e[1])) for e in send["sent_video"]]
        if len({i for i, _ in sent}) != len(sent):
            print(f"{tag}: skipped, a frame index was sent twice (run longer than the corpus)")
            continue
        key = (meta["condition"], meta["system"], meta["loss_per_mille"] / 10)
        total = totals.setdefault(key, [0, 0, 0, 0])
        lo, hi = sent[0][1] + EDGE_START, sent[-1][1] - EDGE_END
        send_time = dict(sent)
        counted = [i for i, t in sent if lo <= t <= hi]
        got: dict[int, tuple[float, float]] = {}
        for e in recv["recv_video"]:
            got.setdefault(int(e[1]), (float(e[2]), float(e[3])))
        lost = [i for i in counted if i not in got]
        cells = []
        total[0] += len(lost)
        for i in lost:
            nxt = got.get(i + 1)
            if nxt is None:
                cells.append("next lost")
                total[1] += 1
                continue
            done, first = nxt
            total[2 if 1000 * (done - first) <= QUICK_MS else 3] += 1
            cells.append(f"{1000 * (done - send_time[i + 1]):.1f}/{1000 * (done - first):.1f}")
        asm = recv.get("assembler") or {}
        print(
            f"{meta['condition']} {meta['system']} {meta['loss_per_mille'] / 10:g}% r{meta['repeat']} "
            f"{len(counted)} {len(lost)} [{asm.get('superseded')} {asm.get('expired')}] | " + " ".join(cells)
        )
    print(
        f"\nsummary: condition system loss | frames lost; the next frame lost too; the next frame "
        f"complete within {QUICK_MS:g} ms of its first fragment; the next frame slower"
    )
    for (condition, system, loss), (n, nxt_lost, quick, slow) in sorted(totals.items()):
        print(f"{condition} {system} {loss:g}% | {n}; {nxt_lost}; {quick}; {slow}")


if __name__ == "__main__":
    main()
