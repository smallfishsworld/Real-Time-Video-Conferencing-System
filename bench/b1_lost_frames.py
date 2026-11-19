"""Which frames did B1's NACK-only runs lose, and what happened to the frame after each?

Usage (from the repository root, after bench/run_b1.sh):

    python3 bench/b1_lost_frames.py [RAW_DIR] > bench/results/b1_lost_frames.txt

RAW_DIR defaults to bench/tmp/b1, where run_b1.sh leaves each run's sender and receiver
records. For every lossy run of a system with NACK it lists the frames of the counted
window (the same window as bench/analyze_b1.py) that never arrived complete, and for
each the frame that followed it: whether it arrived, its latency (send to complete) and
its assembly time (first fragment to complete). A next frame that was complete within
about a millisecond of its first fragment needed no retransmission, so it finished
before any retransmission for the frame before it could arrive; the receiver's
newest-wins assembler then discards the older frame.
"""

from __future__ import annotations

import json
import sys
from datetime import UTC, datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
EDGE_START, EDGE_END = 1.0, 1.5  # as in bench/analyze_b1.py


def main() -> None:
    raw = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "bench" / "tmp" / "b1"
    # Which run the raw records belong to: run_b1.sh rewrites them on every run.
    build = json.loads((ROOT / "bench" / "results" / "b1_loss.json").read_text())["environment"].get("build")
    times = sorted(p.stat().st_mtime for p in raw.glob("*.recv.json"))
    stamp = [datetime.fromtimestamp(t, UTC).strftime("%Y-%m-%dT%H:%M:%SZ") for t in (times[0], times[-1])]
    print(f"raw records: {raw}, written {stamp[0]} to {stamp[1]}")
    print(f"bench/results/b1_loss.json binaries: {build}")
    print("system loss counted lost | per lost frame: next frame latency ms / assembly ms")
    runs = [p.name.removesuffix(".send.json").rsplit("-", 1) for p in raw.glob("*nack-*.send.json")]
    for system, loss in sorted(runs, key=lambda r: (r[0], int(r[1]))):
        if loss == "0":
            continue
        tag = f"{system}-{loss}"
        path = raw / f"{tag}.send.json"
        send = json.loads(path.read_text())
        recv = json.loads((raw / f"{tag}.recv.json").read_text())
        sent = [(int(e[0]), float(e[1])) for e in send["sent_video"]]
        lo, hi = sent[0][1] + EDGE_START, sent[-1][1] - EDGE_END
        send_time = dict(sent)
        counted = [i for i, t in sent if lo <= t <= hi]
        got: dict[int, tuple[float, float]] = {}
        for e in recv["recv_video"]:
            got.setdefault(int(e[1]), (float(e[2]), float(e[3])))
        lost = [i for i in counted if i not in got]
        cells = []
        for i in lost:
            nxt = got.get(i + 1)
            if nxt is None:
                cells.append("next lost")
                continue
            done, first = nxt
            cells.append(f"{1000 * (done - send_time[i + 1]):.1f}/{1000 * (done - first):.1f}")
        print(f"{system} {int(loss) / 10:g}% {len(counted)} {len(lost)} | " + " ".join(cells))


if __name__ == "__main__":
    main()
