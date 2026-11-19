"""Write the benchmark frame corpus: the client's synthetic test pattern at the legacy
camera settings (480x480, JPEG quality 85, encoded with Pillow like the original
client), each frame tagged with its index in a COM segment.

Usage: python bench/gen_frames.py [--count 1000]
Writes bench/data/frames/frame_NNNNN.jpg (not committed; deterministic) and the frame
size distribution to bench/results/frames.json.
"""

from __future__ import annotations

import argparse
import hashlib
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from confclient.testpattern import TestPattern, encode_jpeg

from common import FRAMES_DIR, RESULTS, environment, make_com, summary, write_json


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--count", type=int, default=1000)
    ap.add_argument("--width", type=int, default=480)
    ap.add_argument("--height", type=int, default=480)
    ap.add_argument("--quality", type=int, default=85)
    args = ap.parse_args()

    FRAMES_DIR.mkdir(parents=True, exist_ok=True)
    pattern = TestPattern(args.width, args.height)
    sizes = []
    corpus = hashlib.sha256()
    for n in range(args.count):
        path = FRAMES_DIR / f"frame_{n:05d}.jpg"
        jpeg = encode_jpeg(pattern.frame(n), args.quality)
        tagged = jpeg[:2] + make_com(n) + jpeg[2:]
        path.write_bytes(tagged)
        sizes.append(len(tagged))
        corpus.update(tagged)
    for stale in sorted(FRAMES_DIR.glob("frame_*.jpg"))[args.count :]:
        stale.unlink()
    hist: dict[str, int] = {}
    for s in sizes:
        key = f"{s // 1000}k"
        hist[key] = hist.get(key, 0) + 1
    result = {
        "description": "frame corpus used by every benchmark (synthetic test pattern, Pillow JPEG)",
        "command": (
            f"python bench/gen_frames.py --count {args.count} --width {args.width} "
            f"--height {args.height} --quality {args.quality}"
        ),
        # SHA-256 of every frame's bytes, concatenated in index order.
        "corpus_sha256": corpus.hexdigest(),
        "width": args.width,
        "height": args.height,
        "quality": args.quality,
        "count": args.count,
        "bytes": summary(sizes) | {"min": min(sizes), "p05": sorted(sizes)[len(sizes) // 20]},
        "histogram_kb": dict(sorted(hist.items())),
        "fragments_v2_1200B": summary([-(-s // 1200) for s in sizes]),
        "environment": environment(),
    }
    write_json(RESULTS / "frames.json", result)
    print(f"wrote {args.count} frames to {FRAMES_DIR}; mean {result['bytes']['mean']:.0f} B")


if __name__ == "__main__":
    main()
