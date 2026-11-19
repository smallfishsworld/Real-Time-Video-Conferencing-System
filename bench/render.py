"""Render bench/results/*.json into Markdown tables and PNG plots.

Usage (from the repository root):

    (cd client && uv run --group bench python ../bench/render.py)

Writes bench/results/plots/<name>.png and <name>_dark.png, bench/results/tables.md, and
replaces the blocks between "<!-- BEGIN GENERATED <table> -->" and
"<!-- END GENERATED <table> -->" in README.md and docs/BENCHMARKS.md. Every number in
the output is read from a results file; nothing is computed except ratios and unit
conversions of measured values.
"""

from __future__ import annotations

import itertools
import json
import math
import re
import sys
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import matplotlib as mpl

mpl.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.axes import Axes
from matplotlib.figure import Figure
from matplotlib.lines import Line2D
from matplotlib.patches import PathPatch
from matplotlib.path import Path as MplPath
from matplotlib.ticker import FuncFormatter, LogLocator, MultipleLocator, NullFormatter

ROOT = Path(__file__).resolve().parents[1]
RESULTS = ROOT / "bench" / "results"
PLOTS = RESULTS / "plots"

# --- theme ---------------------------------------------------------------------------
# Chart chrome and the first three categorical slots of the validated reference
# palette (dataviz skill, references/palette.md); colors follow the system, never the
# rank, and are the same in every chart.

LABELS = {
    "legacy": "legacy (original)",
    "v2": "v2",
    "v2-fec": "v2 with FEC",
    "v2-nofec": "v2 without FEC",
    "v2-unbatched": "v2 before syscall batching",
}


@dataclass(frozen=True)
class Theme:
    name: str
    surface: str
    ink: str
    ink2: str
    muted: str
    grid: str
    baseline: str
    series: dict[str, str]


LIGHT = Theme(
    "light",
    surface="#fcfcfb",
    ink="#0b0b0b",
    ink2="#52514e",
    muted="#898781",
    grid="#e1e0d9",
    baseline="#c3c2b7",
    series={
        "legacy": "#2a78d6",
        "v2": "#eb6834",
        "v2-fec": "#eb6834",
        "v2-nofec": "#1baf7a",
        "v2-unbatched": "#1baf7a",
    },
)
DARK = Theme(
    "dark",
    surface="#1a1a19",
    ink="#f0efec",
    ink2="#c3c2b7",
    muted="#898781",
    grid="#2c2c2a",
    baseline="#383835",
    series={
        "legacy": "#3987e5",
        "v2": "#d95926",
        "v2-fec": "#d95926",
        "v2-nofec": "#199e70",
        "v2-unbatched": "#199e70",
    },
)

# Images are 1280 px wide and shown at about 800 CSS px, so one CSS px is 1.6 image
# px; at 200 dpi that is 0.576 pt.
DPI = 200
PT_PER_PX = 1.6 * 72 / DPI


def px(n: float) -> float:
    return n * PT_PER_PX


def load(name: str) -> dict[str, Any] | None:
    path = RESULTS / name
    if not path.exists():
        print(f"skip: {path} not found")
        return None
    data: dict[str, Any] = json.loads(path.read_text())
    return data


# --- chart scaffolding -----------------------------------------------------------


def new_chart(theme: Theme, title: str, subtitle: str, ncols: int = 1) -> tuple[Figure, list[Axes]]:
    fig, axes = plt.subplots(1, ncols, figsize=(6.4, 3.5), dpi=DPI, sharey=True, squeeze=False)
    fig.patch.set_facecolor(theme.surface)
    fig.subplots_adjust(left=0.1, right=0.97, top=0.72, bottom=0.15, wspace=0.08)
    fig.text(0.1, 0.94, title, color=theme.ink, fontsize=px(15), fontweight="bold", va="top")
    fig.text(0.1, 0.875, subtitle, color=theme.ink2, fontsize=px(11.5), va="top")
    out = list(axes[0])
    for ax in out:
        ax.set_facecolor(theme.surface)
        for side in ("top", "right", "left"):
            ax.spines[side].set_visible(False)
        ax.spines["bottom"].set_color(theme.baseline)
        ax.spines["bottom"].set_linewidth(px(1))
        ax.grid(axis="y", color=theme.grid, linewidth=px(1), linestyle="-")
        ax.set_axisbelow(True)
        ax.tick_params(colors=theme.muted, labelsize=px(11), length=0, pad=px(6))
        ax.tick_params(axis="x", length=px(4), width=px(1), color=theme.baseline)
        ax.xaxis.label.set_color(theme.ink2)
        ax.yaxis.label.set_color(theme.ink2)
        ax.xaxis.label.set_fontsize(px(11.5))
        ax.yaxis.label.set_fontsize(px(11.5))
    return fig, out


def legend(fig: Figure, theme: Theme, keys: Sequence[str], kind: str = "line") -> None:
    handles = []
    for k in keys:
        if kind == "line":
            handles.append(
                Line2D(
                    [],
                    [],
                    color=theme.series[k],
                    linewidth=px(2),
                    marker="o",
                    markersize=px(8),
                    markeredgecolor=theme.surface,
                    markeredgewidth=px(2),
                )
            )
        else:
            handles.append(Line2D([], [], color=theme.series[k], linewidth=0, marker="s", markersize=px(10)))
    leg = fig.legend(
        handles,
        [LABELS[k] for k in keys],
        loc="upper left",
        bbox_to_anchor=(0.093, 0.83),
        ncol=len(keys),
        frameon=False,
        fontsize=px(11.5),
        handlelength=2.2 if kind == "line" else 1.0,
        columnspacing=1.6,
    )
    for text in leg.get_texts():
        text.set_color(theme.ink2)


def plot_line(ax: Axes, theme: Theme, key: str, xs: Sequence[float], ys: Sequence[float]) -> None:
    ax.plot(
        xs,
        ys,
        color=theme.series[key],
        linewidth=px(2),
        solid_joinstyle="round",
        solid_capstyle="round",
        marker="o",
        markersize=px(8),
        markeredgecolor=theme.surface,
        markeredgewidth=px(2),
        zorder=3,
    )


def end_labels(
    ax: Axes, theme: Theme, points: dict[str, tuple[float, float]], fmt: Callable[[float], str]
) -> None:
    """Value labels at line ends, only when none of them would collide (else legend only)."""
    fig = ax.figure
    fig.canvas.draw()
    ys = sorted(ax.transData.transform(p)[1] for p in points.values())
    min_sep = 14 * 1.6  # CSS px -> image px
    if any(b - a < min_sep for a, b in itertools.pairwise(ys)):
        return
    for x, y in points.values():
        ax.annotate(
            fmt(y),
            (x, y),
            xytext=(px(8) * DPI / 72, 0),
            textcoords="offset pixels",
            va="center",
            ha="left",
            color=theme.ink2,
            fontsize=px(11),
            annotation_clip=False,
        )


def rounded_bar(ax: Axes, theme: Theme, key: str, x: float, width_px: float, height: float) -> None:
    """A column with a 4 px rounded data end and a square baseline (limits must be final)."""
    to_disp = ax.transData.transform
    to_data = ax.transData.inverted().transform
    scale = 1.6  # CSS px -> image px
    x0, y0 = to_disp((x, 0))
    _, y1 = to_disp((x, height))
    half = width_px * scale / 2
    r = min(4 * scale, abs(y1 - y0), half)
    left, right = x0 - half, x0 + half
    verts = [
        (left, y0),
        (left, y1 - r),
        (left, y1),
        (left + r, y1),
        (right - r, y1),
        (right, y1),
        (right, y1 - r),
        (right, y0),
        (left, y0),
    ]
    codes = [
        MplPath.MOVETO,
        MplPath.LINETO,
        MplPath.CURVE3,
        MplPath.CURVE3,
        MplPath.LINETO,
        MplPath.CURVE3,
        MplPath.CURVE3,
        MplPath.LINETO,
        MplPath.CLOSEPOLY,
    ]
    data_verts = [tuple(to_data(v)) for v in verts]
    ax.add_patch(
        PathPatch(MplPath(data_verts, codes), facecolor=theme.series[key], edgecolor="none", zorder=3)
    )


def grouped_columns(
    fig: Figure,
    ax: Axes,
    theme: Theme,
    groups: list[str],
    keys: list[str],
    values: dict[str, list[float | None]],
    fmt: Callable[[float], str],
    ymax: float,
) -> None:
    ax.set_xlim(-0.6, len(groups) - 0.4)
    ax.set_ylim(0, ymax)
    ax.set_xticks(range(len(groups)), groups)
    fig.canvas.draw()
    width_px, gap_px = 24, 2
    to_data_x = ax.transData.inverted().transform
    step = (width_px + gap_px) * 1.6  # image px between column centres
    for gi in range(len(groups)):
        cx = ax.transData.transform((gi, 0))[0]
        for ki, key in enumerate(keys):
            value = values[key][gi]
            if value is None:
                continue
            offset = (ki - (len(keys) - 1) / 2) * step
            x = to_data_x((cx + offset, 0))[0]
            rounded_bar(ax, theme, key, x, width_px, value)
            ax.annotate(
                fmt(value),
                (x, value),
                xytext=(0, px(5) * DPI / 72),
                textcoords="offset pixels",
                ha="center",
                va="bottom",
                color=theme.ink2,
                fontsize=px(11),
            )


def save(fig: Figure, name: str, theme: Theme) -> None:
    PLOTS.mkdir(parents=True, exist_ok=True)
    suffix = "" if theme.name == "light" else "_dark"
    fig.savefig(PLOTS / f"{name}{suffix}.png", facecolor=theme.surface)
    plt.close(fig)


def percent_axis(ax: Axes, step: float = 20) -> None:
    ax.yaxis.set_major_locator(MultipleLocator(step))
    ax.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:.0f} %"))


# --- B1 ------------------------------------------------------------------------------

B1_ORDER = ["legacy", "v2-fec", "v2-nofec"]


def b1_runs(b1: dict[str, Any]) -> dict[str, list[dict[str, Any]]]:
    out: dict[str, list[dict[str, Any]]] = {}
    for run in b1["runs"]:
        out.setdefault(run["system"], []).append(run)
    for runs in out.values():
        runs.sort(key=lambda r: r["loss_percent"])
    return out


def plot_b1(b1: dict[str, Any], theme: Theme) -> None:
    runs = b1_runs(b1)
    keys = [k for k in B1_ORDER if k in runs]
    fig, (ax,) = new_chart(
        theme,
        "Video frames delivered under random packet loss",
        "Complete frames within 200 ms; 480x480 JPEG at 15.6 fps over a 1500-byte-MTU link",
    )
    legend(fig, theme, keys)
    ends = {}
    for k in keys:
        xs = [r["loss_percent"] for r in runs[k]]
        ys = [100 * r["video"]["delivery_ratio"] for r in runs[k]]
        plot_line(ax, theme, k, xs, ys)
        ends[k] = (xs[-1], ys[-1])
    ax.set_ylim(0, 105)
    ax.set_xlim(-0.3, 11.5)
    percent_axis(ax)
    ax.set_xticks([0, 1, 2, 5, 10], ["0 %", "1 %", "2 %", "5 %", "10 %"])
    ax.set_xlabel("Packet loss on the server-to-receiver link")
    end_labels(ax, theme, ends, lambda v: f"{v:.0f} %")
    save(fig, "b1_video_delivery", theme)

    fig, (ax,) = new_chart(
        theme,
        "Audio lost under random packet loss",
        "Share of the talker's audio frames never played (legacy: 64 ms chunks; v2: 20 ms frames)",
    )
    legend(fig, theme, keys)
    ends = {}
    for k in keys:
        xs = [r["loss_percent"] for r in runs[k]]
        ys = [100 * r["audio"]["residual_loss"] for r in runs[k]]
        plot_line(ax, theme, k, xs, ys)
        ends[k] = (xs[-1], ys[-1])
    ax.set_ylim(0, 80)
    ax.set_xlim(-0.3, 11.5)
    percent_axis(ax)
    ax.set_xticks([0, 1, 2, 5, 10], ["0 %", "1 %", "2 %", "5 %", "10 %"])
    ax.set_xlabel("Packet loss on the server-to-receiver link")
    end_labels(ax, theme, ends, lambda v: f"{v:.1f} %")
    save(fig, "b1_audio_loss", theme)


def pct(v: float | None, digits: int = 1) -> str:
    return "n/a" if v is None else f"{100 * v:.{digits}f} %"


def table_b1(b1: dict[str, Any]) -> str:
    runs = b1_runs(b1)
    keys = [k for k in B1_ORDER if k in runs]
    losses = sorted({r["loss_percent"] for rs in runs.values() for r in rs})
    by = {(k, r["loss_percent"]): r for k in keys for r in runs[k]}
    lines = [
        "| Packet loss | " + " | ".join(f"{LABELS[k]}: video frames on time (model)" for k in keys) + " |",
        "|---" * (len(keys) + 1) + "|",
    ]
    for loss in losses:
        cells = []
        for k in keys:
            r = by.get((k, loss))
            if r is None:
                cells.append("n/a")
                continue
            v = r["video"]
            cells.append(f"{pct(v['delivery_ratio'])} ({pct(v['model_ratio_independent_loss'])})")
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " |")
    lines += [
        "",
        "| Packet loss | " + " | ".join(f"{LABELS[k]}: audio lost" for k in keys) + " | measured UDP loss |",
        "|---" * (len(keys) + 2) + "|",
    ]
    for loss in losses:
        cells = []
        measured = []
        for k in keys:
            r = by.get((k, loss))
            if r is None:
                cells.append("n/a")
                continue
            cells.append(pct(r["audio"]["residual_loss"], 2))
            measured.append(pct(r["measured_udp_packet_loss"], 2))
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " | " + " / ".join(measured) + " |")
    lines += [
        "",
        "| System | Packet loss | Video frames on time, first half | second half | IP reassembly failures"
        " | peak reassembly memory |",
        "|---|---|---|---|---|---|",
    ]
    for k in keys:
        for r in runs[k]:
            v, kern = r["video"], r["receiver_kernel"]
            peak = r.get("receiver_frag_memory", {}).get("max_bytes")
            peak_s = "n/a" if peak is None else f"{peak / 2**20:.2f} MiB"
            lines.append(
                f"| {LABELS[k]} | {r['loss_percent']:g} % | {pct(v['delivery_ratio_first_half'])} | "
                f"{pct(v['delivery_ratio_second_half'])} | {kern['ip_reasm_fails']} | {peak_s} |"
            )
    return "\n".join(lines)


# --- B2 ------------------------------------------------------------------------------


def b2_index(b2: dict[str, Any]) -> dict[tuple[str, int], dict[str, Any]]:
    return {(r["system"], r["participants"]): r for r in b2["runs"]}


def plot_b2(b2: dict[str, Any], theme: Theme) -> None:
    idx = b2_index(b2)
    sizes = sorted({n for _, n in idx})
    keys = [k for k in ("legacy", "v2") if any((k, n) in idx for n in sizes)]
    groups = [f"{n} parties" for n in sizes]

    def series(get: Callable[[dict[str, Any]], float]) -> dict[str, list[float | None]]:
        return {k: [get(idx[(k, n)]) if (k, n) in idx else None for n in sizes] for k in keys}

    charts = [
        (
            "b2_audio_latency",
            "Mouth-to-speaker audio latency",
            "Median over all receivers; everybody talks and sends video",
            series(lambda r: r["audio_latency_ms"]["p50"]),
            lambda v: f"{v:.0f}",
            "Latency (ms)",
        ),
        (
            "b2_downstream_audio",
            "Downstream audio bandwidth per receiver",
            "IP-level bit rate into each receiver; v2 includes its audio FEC (k=2)",
            series(lambda r: r["downstream_audio_kbit_per_s_mean"]),
            lambda v: f"{v:.0f}",
            "kbit/s",
        ),
        (
            "b2_video_shown",
            "Video frames that reached the receiver application",
            "Share of the other participants' frames that the receiving client processed",
            series(lambda r: 100 * _mean(rc["video"]["delivery_ratio"] for rc in r["receivers"])),
            lambda v: f"{v:.0f}%",
            "",
        ),
    ]
    for name, title, subtitle, values, fmt, ylabel in charts:
        fig, (ax,) = new_chart(theme, title, subtitle)
        legend(fig, theme, keys, kind="bar")
        top = max(v for vs in values.values() for v in vs if v is not None)
        ymax = 105 if name == "b2_video_shown" else _nice_ceiling(top * 1.18)
        grouped_columns(fig, ax, theme, groups, keys, values, fmt, ymax)
        if name == "b2_video_shown":
            percent_axis(ax)
        ax.set_ylabel(ylabel)
        save(fig, name, theme)


def _mean(xs: Any) -> float:
    vals = list(xs)
    return sum(vals) / len(vals)


def _nice_ceiling(v: float) -> float:
    """The smallest 'round' number (1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8 x 10^k) >= v."""
    mag = 10 ** math.floor(math.log10(v))
    return next((m * mag for m in (1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8) if m * mag >= v), 10 * mag)


def table_b2(b2: dict[str, Any]) -> str:
    lines = [
        "| System | Parties | Audio latency p50 / p95 (ms) | Audio delivered (worst talker) "
        "| Downstream audio per receiver (kbit/s) | Video frames processed | Longest video freeze "
        "| Video time frozen > 0.5 s |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for (system, n), r in sorted(b2_index(b2).items(), key=lambda kv: (kv[0][0] != "legacy", kv[0][1])):
        recs = r["receivers"]
        worst = min(min(rc["audio_delivery_by_talker"].values()) for rc in recs)
        down = r["downstream_audio_kbit_per_s_mean"]
        nofec = [rc["downstream_audio"].get("kbit_per_s_without_fec") for rc in recs]
        down_s = f"{down:.0f}"
        if all(x is not None for x in nofec):
            down_s += f" ({_mean(nofec):.0f} without FEC)"
        video = _mean(rc["video"]["delivery_ratio"] for rc in recs)
        gap = max(rc["video"]["longest_gap_s"] for rc in recs)
        stalled = _mean(rc["video"]["stalled_fraction"] for rc in recs)
        lat = r["audio_latency_ms"]
        lines.append(
            f"| {LABELS[system]} | {n} | {lat['p50']:.0f} / {lat['p95']:.0f} | {pct(worst)} | {down_s} | "
            f"{pct(video)} | {gap:.2f} s | {pct(stalled)} |"
        )
    return "\n".join(lines)


# --- B3 ------------------------------------------------------------------------------


B3_ORDER = ["legacy", "v2", "v2-unbatched"]


def plot_b3(variants: list[tuple[str, dict[str, Any]]], theme: Theme) -> None:
    keys = [k for k in B3_ORDER if any(k in b3["systems"] for _, b3 in variants)]
    metrics = [
        (
            "b3_delivery",
            "Relay capacity: frame delivery",
            "Rooms of 4, 15 fps per participant, server pinned to one CPU",
            lambda r: 100 * r["delivery_ratio"],
            (0, 105),
            "linear",
        ),
        (
            "b3_p99_latency",
            "Relay capacity: 99th-percentile frame latency",
            "Send to last byte received; the pass limit is 100 ms",
            lambda r: max(r["latency_ms"].get("p99", float("nan")), 0.1),
            (0.1, 20000),
            "log",
        ),
        (
            "b3_server_cpu",
            "Relay capacity: server CPU",
            "CPU time of the server process / wall time (1.0 = the one CPU it is pinned to)",
            lambda r: r["server_cpu_utilisation"],
            (0, 1.1),
            "linear",
        ),
    ]
    for name, title, subtitle, get, ylim, scale in metrics:
        fig, axes = new_chart(theme, title, subtitle, ncols=len(variants))
        legend(fig, theme, keys)
        xmax = max(
            r["participants"] for _, b3 in variants for s in b3["systems"].values() for r in s["sweep"]
        )
        for ax, (label, b3) in zip(axes, variants, strict=True):
            for k in keys:
                s = b3["systems"].get(k)
                if not s:
                    continue
                xs = [r["participants"] for r in s["sweep"]]
                ys = [get(r) for r in s["sweep"]]
                plot_line(ax, theme, k, xs, ys)
            ax.set_yscale(scale)
            ax.set_ylim(*ylim)
            ax.set_xlim(0, xmax * 1.05)
            ax.set_title(label, color=theme.ink2, fontsize=px(11.5), loc="left", pad=px(6))
            ax.set_xlabel("Participants")
            if name == "b3_delivery":
                percent_axis(ax)
            if name == "b3_p99_latency":
                ax.yaxis.set_major_locator(LogLocator(base=10))
                ax.yaxis.set_minor_formatter(NullFormatter())
                ax.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g} ms"))
                ax.axhline(100, color=theme.muted, linewidth=px(1), linestyle=(0, (4, 3)), zorder=2)
            if name == "b3_server_cpu":
                ax.yaxis.set_major_locator(MultipleLocator(0.25))
        save(fig, name, theme)


def table_b3(variants: list[tuple[str, dict[str, Any]]]) -> str:
    lines = [
        "| Loopback MTU | System | Capacity (participants) | First failing | CPU s per participant-minute"
        " (fixed run) | Server CPU in fixed run | Load generator CPU in fixed run |",
        "|---|---|---|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            fixed = s.get("fixed") or {}
            first = s.get("first_failing_participants")
            cap = s["capacity_participants"]
            last_swept = max(r["participants"] for r in s["sweep"])
            cap_s = f"{cap}" if first is not None else f">= {cap} (largest size swept: {last_swept})"
            lines.append(
                f"| {label} | {LABELS[k]} | {cap_s} | {first if first is not None else 'none'} | "
                f"{_num(fixed.get('server_cpu_seconds_per_participant_minute'), 3)} "
                f"(N={fixed.get('participants', 'n/a')}) | {_num(fixed.get('server_cpu_utilisation'), 3)} | "
                f"{_num(fixed.get('loadgen_cpu_utilisation'), 2)} |"
            )
    lines += [
        "",
        "| Loopback MTU | System | Participants | Offered upstream (Mbit/s) | Frame delivery "
        "| p50 / p99 latency (ms) | Audio delivery | Server CPU | Load generator CPU | Pass |",
        "|---|---|---|---|---|---|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            for r in s["sweep"]:
                lat = r["latency_ms"]
                lines.append(
                    f"| {label} | {LABELS[k]} | {r['participants']} | {r['offered_upstream_mbps']:.0f} | "
                    f"{pct(r['delivery_ratio'], 2)} | "
                    f"{_num(lat.get('p50'), 1)} / {_num(lat.get('p99'), 1)} | "
                    f"{pct(r.get('audio_delivery_ratio'), 1)} | {_num(r['server_cpu_utilisation'], 2)} | "
                    f"{_num(r['loadgen_cpu_utilisation'], 2)} | {'yes' if r['pass'] else 'no'} |"
                )
    return "\n".join(lines)


def _num(v: float | None, digits: int) -> str:
    return "n/a" if v is None else f"{v:.{digits}f}"


# --- micro ---------------------------------------------------------------------------


def table_micro(micro: dict[str, Any]) -> str:
    lines = ["| Benchmark | ns/op (median of 5) | MB/s | B/op | allocs/op |", "|---|---|---|---|---|"]
    for b in micro["benchmarks"]:
        m = b["median"]
        mbs = f"{m['MB/s']:.0f}" if "MB/s" in m else ""
        extra = f" ({m['packets/op']:.2f} packets/op)" if "packets/op" in m else ""
        lines.append(
            f"| `{b['name']}`{extra} | {m['ns/op']:.1f} | {mbs} | "
            f"{m.get('B/op', 0):.0f} | {m.get('allocs/op', 0):.0f} |"
        )
    return "\n".join(lines)


# --- README summaries ----------------------------------------------------------------


def summary_b1(b1: dict[str, Any]) -> str:
    runs = b1_runs(b1)
    keys = [k for k in B1_ORDER if k in runs]
    by = {(k, r["loss_percent"]): r for k in keys for r in runs[k]}
    losses = sorted({r["loss_percent"] for rs in runs.values() for r in rs if r["loss_percent"] > 0})
    lines = [
        "| Random packet loss | " + " | ".join(LABELS[k] for k in keys) + " |",
        "|---" * (len(keys) + 1) + "|",
    ]
    for loss in losses:
        cells = [pct(by[(k, loss)]["video"]["delivery_ratio"]) if (k, loss) in by else "n/a" for k in keys]
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " |")
    return "\n".join(lines)


def summary_b2(b2: dict[str, Any]) -> str:
    idx = b2_index(b2)
    cols = sorted(idx, key=lambda k: (k[1], k[0] != "legacy"))
    rows: list[tuple[str, Callable[[dict[str, Any]], str]]] = [
        ("Audio latency, median", lambda r: f"{r['audio_latency_ms']['p50']:.0f} ms"),
        (
            "Audio played, worst talker",
            lambda r: pct(min(min(rc["audio_delivery_by_talker"].values()) for rc in r["receivers"])),
        ),
        ("Downstream audio per receiver", lambda r: f"{r['downstream_audio_kbit_per_s_mean']:.0f} kbit/s"),
        (
            "Video frames processed",
            lambda r: pct(_mean(rc["video"]["delivery_ratio"] for rc in r["receivers"])),
        ),
    ]
    head = "| | " + " | ".join(f"{n} parties, {LABELS[s]}" for s, n in cols) + " |"
    lines = [head, "|---" * (len(cols) + 1) + "|"]
    for name, get in rows:
        lines.append(f"| {name} | " + " | ".join(get(idx[c]) for c in cols) + " |")
    return "\n".join(lines)


def summary_b3(variants: list[tuple[str, dict[str, Any]]]) -> str:
    lines = [
        "| Loopback MTU | System | Participants sustained (largest size swept that passed) "
        "| Server CPU-seconds per participant-minute (16 participants) |",
        "|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            first = s.get("first_failing_participants")
            cap = s["capacity_participants"]
            cap_s = f"{cap} (fails at {first})" if first is not None else f"{cap} (every size swept passed)"
            fixed = s.get("fixed") or {}
            lines.append(
                f"| {label} | {LABELS[k]} | {cap_s} | "
                f"{_num(fixed.get('server_cpu_seconds_per_participant_minute'), 3)} |"
            )
    return "\n".join(lines)


# --- output --------------------------------------------------------------------------


def replace_blocks(path: Path, blocks: dict[str, str]) -> None:
    if not path.exists():
        return
    text = path.read_text()
    for name, body in blocks.items():
        pattern = re.compile(
            rf"(<!-- BEGIN GENERATED {re.escape(name)} -->\n)(?:.*?\n)?"
            rf"(<!-- END GENERATED {re.escape(name)} -->)",
            re.DOTALL,
        )
        text = pattern.sub(lambda m, body=body: m.group(1) + body + "\n" + m.group(2), text)
    path.write_text(text)


def main() -> None:
    plt.rcParams["font.family"] = "DejaVu Sans"
    blocks: dict[str, str] = {}
    b1 = load("b1_loss.json")
    b2 = load("b2_audio.json")
    variants = [
        (label, data)
        for label, data in (
            ("MTU 65536 (Linux default)", load("b3_capacity.json")),
            ("MTU 1500 (Ethernet)", load("b3_capacity_mtu1500.json")),
        )
        if data is not None
    ]
    unbatched = load("b3_capacity_unbatched.json")
    if variants and unbatched is not None and "v2" in unbatched["systems"]:
        main_b3 = variants[0][1]
        if main_b3["environment"]["git_commit"] != unbatched["environment"]["git_commit"]:
            main_b3["systems"]["v2-unbatched"] = unbatched["systems"]["v2"]
    micro = load("micro.json")
    for theme in (LIGHT, DARK):
        if b1:
            plot_b1(b1, theme)
        if b2:
            plot_b2(b2, theme)
        if variants:
            plot_b3(variants, theme)
    if b1:
        blocks["b1"] = table_b1(b1)
        blocks["summary_b1"] = summary_b1(b1)
    if b2:
        blocks["b2"] = table_b2(b2)
        blocks["summary_b2"] = summary_b2(b2)
    if variants:
        blocks["b3"] = table_b3(variants)
        blocks["summary_b3"] = summary_b3(variants)
    if micro:
        blocks["micro"] = table_micro(micro)
    (RESULTS / "tables.md").write_text(
        "# Generated benchmark tables\n\n"
        "Generated by `bench/render.py` from the JSON files in this directory.\n\n"
        + "\n\n".join(f"## {name}\n\n{body}" for name, body in blocks.items())
        + "\n"
    )
    for doc in (ROOT / "README.md", ROOT / "docs" / "BENCHMARKS.md"):
        replace_blocks(doc, blocks)
    print(f"wrote {len(list(PLOTS.glob('*.png')))} plots and {len(blocks)} tables")
    sys.exit(0)


if __name__ == "__main__":
    main()
