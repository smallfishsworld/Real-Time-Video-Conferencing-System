"""Shared pieces of the benchmark harnesses: the frame corpus, frame and audio tags,
statistics and result files.

Every system under test sends the same JPEG frames (bench/data/frames, written by
gen_frames.py). Each frame carries its index in a JPEG COM segment at a fixed offset,
so a receiver can identify a frame from its bytes without decoding it. Audio frames
carry tags in reserved sample slots that survive the v2 server's mixing: talker i
writes its tag into samples [4i, 4i+4) and zeros into every other reserved slot.
"""

from __future__ import annotations

import contextlib
import gzip
import json
import math
import os
import platform
import subprocess
import sys
import time
from collections.abc import Iterable, Sequence
from pathlib import Path
from typing import Any

import numpy as np
import numpy.typing as npt

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "bench" / "data"
FRAMES_DIR = DATA / "frames"
RESULTS = ROOT / "bench" / "results"

# JPEG COM segment right after SOI: FF FE, length 34, then 32 bytes of text.
COM_OFFSET = 6
COM_LEN = 32
COM_PREFIX = b"cs305 frame "


def make_com(index: int) -> bytes:
    text = COM_PREFIX + b"%08d" % index
    return b"\xff\xfe" + (COM_LEN + 2).to_bytes(2, "big") + text.ljust(COM_LEN, b" ")


def frame_index(jpeg: bytes | memoryview) -> int | None:
    """The corpus index stored in a frame's COM segment, or None if it has none."""
    tag = bytes(jpeg[COM_OFFSET : COM_OFFSET + COM_LEN])
    if not tag.startswith(COM_PREFIX):
        return None
    try:
        return int(tag[len(COM_PREFIX) : len(COM_PREFIX) + 8])
    except ValueError:
        return None


def load_frames(limit: int | None = None) -> list[bytes]:
    files = sorted(FRAMES_DIR.glob("frame_*.jpg"))
    if not files:
        sys.exit(f"no frame corpus in {FRAMES_DIR}; run bench/gen_frames.py first")
    return [f.read_bytes() for f in files[:limit]]


# Audio tags -------------------------------------------------------------------------

TAG_MAGIC = 0x5A00  # high byte magic, low byte talker index
Pcm = npt.NDArray[np.int16]


def monotonic_ms() -> int:
    return int(time.monotonic() * 1000) & 0xFFFFFFFF


def tagged_pcm(samples: int, talker: int, talkers: int, seq: int, t_ms: int, freq: float, n0: int) -> Pcm:
    """A frame of `samples` samples: a tone, with every reserved slot zeroed and the
    talker's own slot holding (magic|talker, seq, t_ms low, t_ms high)."""
    n = np.arange(n0, n0 + samples)
    pcm = (2000 * np.sin(2 * math.pi * freq * n / 16000)).astype(np.int16)
    pcm[: 4 * talkers] = 0
    u = pcm.view(np.uint16)
    base = 4 * talker
    u[base] = TAG_MAGIC | talker
    u[base + 1] = seq & 0xFFFF
    u[base + 2] = t_ms & 0xFFFF
    u[base + 3] = (t_ms >> 16) & 0xFFFF
    return pcm


def read_tags(pcm: Pcm, talkers: int) -> list[tuple[int, int, int]]:
    """Valid (talker, seq, t_ms) tags in a native-order frame."""
    u = pcm.view(np.uint16)
    out = []
    for i in range(min(talkers, len(u) // 4)):
        if u[4 * i] == TAG_MAGIC | i:
            out.append((i, int(u[4 * i + 1]), int(u[4 * i + 2]) | int(u[4 * i + 3]) << 16))
    return out


def age_ms(now_ms: int, t_ms: int) -> int:
    return (now_ms - t_ms) & 0xFFFFFFFF


# Statistics -----------------------------------------------------------------------------


def percentile(values: Sequence[float], q: float) -> float | None:
    if not values:
        return None
    return float(np.percentile(np.asarray(values, dtype=np.float64), q))


def spread(values: Iterable[float | None]) -> dict[str, Any]:
    """Median, minimum and maximum of repeated measurements (None values are left out)."""
    v = [float(x) for x in values if x is not None]
    if not v:
        return {"n": 0, "median": None, "min": None, "max": None}
    return {"n": len(v), "median": float(np.median(v)), "min": min(v), "max": max(v)}


def wilson(successes: int, trials: int, z: float = 1.959964) -> dict[str, float | int | None]:
    """The proportion and its Wilson score interval (95 % for the default z)."""
    if trials <= 0:
        return {"k": successes, "n": trials, "p": None, "low": None, "high": None}
    p = successes / trials
    denom = 1 + z * z / trials
    centre = (p + z * z / (2 * trials)) / denom
    half = z * math.sqrt(p * (1 - p) / trials + z * z / (4 * trials * trials)) / denom
    low, high = max(0.0, centre - half), min(1.0, centre + half)
    return {"k": successes, "n": trials, "p": p, "low": low, "high": high}


def parse_metrics(text: str) -> dict[str, float]:
    """Prometheus text exposition lines as {name{labels}: value}."""
    out: dict[str, float] = {}
    for line in text.splitlines():
        if line and not line.startswith("#"):
            name, _, value = line.rpartition(" ")
            with contextlib.suppress(ValueError):
                out[name] = float(value)
    return out


def syscall_summary(m: dict[str, float]) -> dict[str, Any]:
    """The v2 server's batching and GSO state and datagrams per system call, from a
    scrape of its /metrics (counters cover the server's whole life)."""
    received = sum(x for k, x in m.items() if k.startswith("confserver_media_packets_received_total"))
    video = sum(
        m.get(f'confserver_media_packets_sent_total{{kind="{kind}"}}', 0.0) for kind in ("video", "video_fec")
    )
    reads = m.get("confserver_media_read_syscalls_total", 0.0)
    calls = m.get("confserver_video_send_syscalls_total", 0.0)
    msgs = m.get("confserver_video_send_messages_total", 0.0)
    batching = m.get("confserver_udp_batching_enabled")
    gso = m.get("confserver_udp_gso_enabled")
    return {
        "udp_batching_enabled": None if batching is None else bool(batching),
        "udp_gso_enabled": None if gso is None else bool(gso),
        "datagrams_received": received,
        "read_syscalls": reads,
        "datagrams_per_read_call": received / reads if reads else None,
        "video_datagrams_sent": video,
        "video_send_syscalls": calls,
        "video_datagrams_per_send_call": video / calls if calls else None,
        "video_send_messages": msgs,
        "video_datagrams_per_message": video / msgs if msgs else None,
        "drops": {
            k.split('reason="', 1)[1].rstrip('"}'): v
            for k, v in m.items()
            if k.startswith("confserver_media_dropped_packets_total{")
        },
    }


def summary(values: Iterable[float]) -> dict[str, float | int | None]:
    v = list(values)
    return {
        "n": len(v),
        "mean": float(np.mean(v)) if v else None,
        "p50": percentile(v, 50),
        "p95": percentile(v, 95),
        "p99": percentile(v, 99),
        "max": max(v) if v else None,
    }


# Result files ---------------------------------------------------------------------------


KERNEL_CONFIG_KEYS = (
    "CONFIG_IRQ_TIME_ACCOUNTING",
    "CONFIG_VIRT_CPU_ACCOUNTING_GEN",
    "CONFIG_TICK_CPU_ACCOUNTING",
    "CONFIG_HZ",
)


def kernel_config() -> dict[str, str]:
    """The CPU-accounting options of the running kernel ("n" when not set), read from
    /proc/config.gz or /boot/config-<release>; empty when neither is readable."""
    text = ""
    with contextlib.suppress(OSError):
        text = gzip.decompress(Path("/proc/config.gz").read_bytes()).decode()
    if not text:
        with contextlib.suppress(OSError):
            text = Path(f"/boot/config-{platform.release()}").read_text()
    if not text:
        return {}
    found = dict.fromkeys(KERNEL_CONFIG_KEYS, "n")
    for line in text.splitlines():
        key, sep, value = line.partition("=")
        if sep and key in found:
            found[key] = value
    return found


def softirq_jiffies(cpu: int) -> int | None:
    """Softirq time of one CPU from /proc/stat, in USER_HZ ticks."""
    with contextlib.suppress(OSError, ValueError, IndexError):
        for line in Path("/proc/stat").read_text().splitlines():
            if line.startswith(f"cpu{cpu} "):
                return int(line.split()[7])
    return None


def environment(build_record: Path | None = None) -> dict[str, Any]:
    """The machine, the kernel's CPU accounting, the tree's state now, and (from
    build_record, written by bench/lib/common.sh write_build_record) what the run's
    binaries were built from."""

    def run(cmd: list[str]) -> str:
        try:
            return subprocess.run(cmd, capture_output=True, text=True, timeout=10, check=False).stdout.strip()
        except OSError:
            return ""

    cpu = ""
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    except OSError:
        pass
    # Results under bench/results are outputs, not inputs, so they do not count.
    dirty = run(
        [
            "git",
            "-C",
            str(ROOT),
            "status",
            "--porcelain",
            "--",
            "server",
            "client",
            "bench",
            "legacy",
            ":!bench/results",
        ]
    )
    build: dict[str, Any] | None = None
    if build_record is not None:
        build = json.loads(build_record.read_text())
    loadavg = ""
    with contextlib.suppress(OSError):
        loadavg = Path("/proc/loadavg").read_text().strip()
    return {
        "kernel": platform.release(),
        # How the kernel charges CPU time: with IRQ_TIME_ACCOUNTING off, softirq work
        # (including loopback receive processing) is charged to whichever task was
        # running when it ran.
        "kernel_config": kernel_config(),
        "loadavg": loadavg,
        "cpu": cpu,
        "cpus": os.cpu_count(),
        "python": platform.python_version(),
        "go": run(["go", "version"]),
        "git_commit": run(["git", "-C", str(ROOT), "rev-parse", "--short", "HEAD"]),
        "git_dirty": bool(dirty),
        # Uncommitted paths when the results were written (right after the runs).
        "git_dirty_files": [line.strip() for line in dirty.splitlines()],
        # The commit, uncommitted paths and binary hashes when this run's binaries were
        # built (bench/lib/common.sh write_build_record), before the runs.
        "build": build,
        "utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }


def write_json(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, indent=2, sort_keys=False) + "\n")
    tmp.replace(path)
