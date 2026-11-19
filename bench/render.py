"""Render bench/results/*.json into Markdown tables and PNG plots.

Usage (from the repository root):

    (cd client && uv run --group bench python ../bench/render.py)

Writes bench/results/plots/<name>.png and <name>_dark.png, bench/results/tables.md, and
replaces the blocks between "<!-- BEGIN GENERATED <table> -->" and
"<!-- END GENERATED <table> -->" in README.md and docs/BENCHMARKS.md. Every number in
the output is read from a results file; nothing is computed except ratios, unit
conversions and the medians and ranges of repeated measurements.

Repeated runs are shown as the median with the range (minimum-maximum) of the runs.
B1's pooled frame ratios carry their Wilson 95 % score interval in square brackets,
which treats every frame as an independent trial (see bench/analyze_b1.py).
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
from matplotlib.ticker import FuncFormatter, LogLocator, MultipleLocator, NullFormatter, NullLocator

ROOT = Path(__file__).resolve().parents[1]
RESULTS = ROOT / "bench" / "results"
PLOTS = RESULTS / "plots"

# --- theme ---------------------------------------------------------------------------
# Chart chrome and the first four categorical slots of a validated colour-blind-safe
# palette (worst adjacent CVD separation dE 9.1 light / 8.4 dark); colours follow the
# system, never the rank, and are the same in every chart. NACK is a second factor on
# top of an FEC setting, so the NACK runs share their FEC setting's colour in a panel
# of their own. The light-mode aqua and yellow are below 3:1 contrast on the surface,
# so every chart has a table with the same numbers.

LABELS = {
    "legacy": "legacy (original)",
    "v2": "v2",
    "v2-fec": "v2 with FEC",
    "v2-nofec": "v2 without FEC",
    "v2-nack": "v2 with NACK",
    "v2-fec-nack": "v2 with FEC and NACK",
    "v2-gso": "v2 (batching and GSO, default)",
    "v2-nogso": "v2, recvmmsg/sendmmsg without GSO",
    "v2-nobatch": "v2, one system call per datagram",
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
        "v2-nack": "#1baf7a",
        "v2-fec-nack": "#eb6834",
        "v2-gso": "#eb6834",
        "v2-nobatch": "#1baf7a",
        "v2-nogso": "#eda100",
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
        "v2-nack": "#199e70",
        "v2-fec-nack": "#d95926",
        "v2-gso": "#d95926",
        "v2-nobatch": "#199e70",
        "v2-nogso": "#c98500",
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


def legend(
    fig: Figure, theme: Theme, keys: Sequence[str], kind: str = "line", labels: dict[str, str] | None = None
) -> None:
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
        [(labels or LABELS)[k] for k in keys],
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
    ranges: dict[str, list[tuple[float, float] | None]] | None = None,
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
            top = value
            span = (ranges or {}).get(key, [None] * len(groups))[gi]
            if span is not None and span[1] > span[0]:
                ax.plot([x, x], span, color=theme.ink2, linewidth=px(1.5), zorder=4, solid_capstyle="butt")
                top = max(top, span[1])
            ax.annotate(
                fmt(value),
                (x, top),
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


def pct(v: float | None, digits: int = 1) -> str:
    return "n/a" if v is None else f"{100 * v:.{digits}f} %"


def _num(v: float | None, digits: int) -> str:
    """v rounded to digits ('n/a' for None), without the sign of a value that rounds to
    zero ('-0')."""
    if v is None:
        return "n/a"
    s = f"{v:.{digits}f}"
    return s[1:] if s.startswith("-") and float(s) == 0 else s


def _nice_ceiling(v: float) -> float:
    """The smallest 'round' number (1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8 x 10^k) >= v."""
    mag = 10.0 ** math.floor(math.log10(v))
    return float(next((m * mag for m in (1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8) if m * mag >= v), 10 * mag))


def ci(w: dict[str, Any], digits: int = 1) -> str:
    """A pooled ratio with its Wilson 95 % interval: '99.5 % [99.1, 99.7]'."""
    if w.get("p") is None:
        return "n/a"
    lo, hi = 100 * w["low"], 100 * w["high"]
    return f"{100 * w['p']:.{digits}f} % [{lo:.{digits}f}, {hi:.{digits}f}]"


def rng(s: dict[str, Any], digits: int = 1, scale: float = 1.0, unit: str = "") -> str:
    """The median of repeated runs and their range: '0.52 (0.51-0.54)', '3 (-7 to 5)'."""
    if s.get("median") is None:
        return "n/a"
    med, lo, hi = (_num(scale * s[k], digits) for k in ("median", "min", "max"))
    if s.get("n", 0) <= 1 or lo == hi:
        return f"{med}{unit}"
    return f"{med}{unit} ({lo}{' to ' if lo.startswith('-') else '-'}{hi})"


def rng_pct(s: dict[str, Any], digits: int = 1) -> str:
    return rng(s, digits, 100.0, " %")


# --- B1 ------------------------------------------------------------------------------

# Tables: each v2 mechanism added in turn (v2-fec-nack is the client's default).
B1_ORDER = ["legacy", "v2-nofec", "v2-fec", "v2-nack", "v2-fec-nack"]
# Charts: one colour per FEC setting, NACK in a panel of its own.
B1_PANELS = [
    ("Without retransmission", ["legacy", "v2-nofec", "v2-fec"]),
    ("v2 with NACK retransmission", ["legacy", "v2-nack", "v2-fec-nack"]),
]
B1_CONDITION_TEXT = {
    "downlink": "server-to-receiver loss only, sub-ms round trip",
    "both-rtt50": "loss in both directions, +50 ms round trip",
}


def b1_groups(b1: dict[str, Any], condition: str) -> dict[str, list[dict[str, Any]]]:
    out: dict[str, list[dict[str, Any]]] = {}
    for g in b1["groups"]:
        if g["condition"] == condition:
            out.setdefault(g["system"], []).append(g)
    for gs in out.values():
        gs.sort(key=lambda g: g["loss_percent"])
    return out


def plot_b1(b1: dict[str, Any], theme: Theme, condition: str, name: str) -> None:
    groups = b1_groups(b1, condition)
    if not groups:
        return
    panels = [(t, [k for k in ks if k in groups]) for t, ks in B1_PANELS]
    panels = [(t, ks) for t, ks in panels if ks]
    keys = list(dict.fromkeys(k for _, ks in panels for k in ks))
    # The colour is the FEC setting: the legend names it for the NACK runs as well.
    fec_setting = {"v2-nack": "v2-nofec", "v2-fec-nack": "v2-fec"}
    shown = {fec_setting.get(k, k) for k in keys}
    legend_keys = [k for k in ("legacy", "v2-nofec", "v2-fec") if k in shown]
    runs = max(g["runs"] for gs in groups.values() for g in gs)
    fig, axes = new_chart(
        theme,
        "Video frames delivered under random packet loss",
        f"Complete within 200 ms; {B1_CONDITION_TEXT.get(condition, condition)}; "
        f"{runs} run{'s' if runs != 1 else ''} pooled, bars: 95 % interval",
        ncols=len(panels),
    )
    legend(fig, theme, legend_keys)
    xmax = max(g["loss_percent"] for gs in groups.values() for g in gs)
    for ax, (title, panel_keys) in zip(axes, panels, strict=True):
        ends: dict[str, tuple[float, float]] = {}
        for k in panel_keys:
            xs = [g["loss_percent"] for g in groups[k]]
            ws = [g["video"]["delivery_ratio"] for g in groups[k]]
            ys = [100 * w["p"] for w in ws]
            for x, w in zip(xs, ws, strict=True):
                ax.plot(
                    [x, x],
                    [100 * w["low"], 100 * w["high"]],
                    color=theme.series[k],
                    linewidth=px(1.5),
                    zorder=2,
                )
            plot_line(ax, theme, k, xs, ys)
            ends[k] = (xs[-1], ys[-1])
        ax.set_ylim(0, 105)
        ax.set_xlim(-0.3, xmax * 1.15 + 0.5)
        percent_axis(ax)
        ticks = sorted({g["loss_percent"] for gs in groups.values() for g in gs})
        ax.set_xticks(ticks, [f"{t:g} %" for t in ticks])
        ax.set_xlabel("Packet loss" + (" per direction" if condition == "both-rtt50" else ""))
        if len(panels) > 1:
            ax.set_title(title, color=theme.ink2, fontsize=px(11.5), loc="left", pad=px(6))
        end_labels(ax, theme, ends, lambda v: f"{v:.0f} %")
    save(fig, name, theme)


def plot_b1_audio(b1: dict[str, Any], theme: Theme) -> None:
    groups = b1_groups(b1, "downlink")
    keys = [k for k in ("legacy", "v2-nofec", "v2-fec") if k in groups]
    if not keys:
        return
    # NACK covers video only; the NACK runs' audio repeats their FEC setting's (tables).
    fig, (ax,) = new_chart(
        theme,
        "Audio lost under random packet loss",
        "Share of the talker's audio frames never played (legacy: 64 ms chunks; v2: 20 ms frames)",
    )
    legend(fig, theme, keys)
    ends: dict[str, tuple[float, float]] = {}
    for k in keys:
        xs = [g["loss_percent"] for g in groups[k]]
        ys = [100 * g["audio"]["residual_loss"]["p"] for g in groups[k]]
        plot_line(ax, theme, k, xs, ys)
        ends[k] = (xs[-1], ys[-1])
    top = max(y for k in keys for y in [100 * g["audio"]["residual_loss"]["p"] for g in groups[k]])
    ax.set_ylim(0, _nice_ceiling(max(top * 1.1, 1)))
    xmax = max(g["loss_percent"] for k in keys for g in groups[k])
    ax.set_xlim(-0.3, xmax * 1.15 + 0.5)
    percent_axis(ax, 20 if top > 40 else 10)
    ticks = sorted({g["loss_percent"] for k in keys for g in groups[k]})
    ax.set_xticks(ticks, [f"{t:g} %" for t in ticks])
    ax.set_xlabel("Packet loss on the server-to-receiver link")
    end_labels(ax, theme, ends, lambda v: f"{v:.1f} %")
    save(fig, "b1_audio_loss", theme)


def _b1_grid(
    groups: dict[str, list[dict[str, Any]]], keys: list[str], cell: Callable[[dict[str, Any]], str], head: str
) -> list[str]:
    by = {(k, g["loss_percent"]): g for k in keys for g in groups[k]}
    losses = sorted({loss for _, loss in by})
    lines = [f"| {head} | " + " | ".join(LABELS[k] for k in keys) + " |", "|---" * (len(keys) + 1) + "|"]
    for loss in losses:
        cells = [cell(by[(k, loss)]) if (k, loss) in by else "n/a" for k in keys]
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " |")
    return lines


def table_b1(b1: dict[str, Any]) -> str:
    groups = b1_groups(b1, "downlink")
    keys = [k for k in B1_ORDER if k in groups]
    frames = sorted({g["video"]["frames_sent"] for gs in groups.values() for g in gs})
    runs = sorted({g["runs"] for gs in groups.values() for g in gs})
    lines = [
        f"Video frames on time, pooled over {'/'.join(map(str, runs))} runs per cell "
        f"({frames[0]}-{frames[-1]} frames), with the Wilson 95 % interval:",
        "",
    ]
    lines += _b1_grid(groups, keys, lambda g: ci(g["video"]["delivery_ratio"]), "Packet loss")
    lines += ["", "The same per run: median (range), and the independent-loss model:", ""]
    lines += _b1_grid(
        groups,
        keys,
        lambda g: (
            f"{rng_pct(g['video']['delivery_ratio_per_run'])}; model "
            f"{pct(g['video']['model_ratio_independent_loss'])}"
        ),
        "Packet loss",
    )
    lines += ["", "Audio frames lost, pooled, with the Wilson 95 % interval:", ""]
    lines += _b1_grid(groups, keys, lambda g: ci(g["audio"]["residual_loss"], 2), "Packet loss")
    lines += ["", "Measured UDP packet loss (nftables drops / matches), median (range) of the runs:", ""]
    lines += _b1_grid(groups, keys, lambda g: rng_pct(g["measured_udp_packet_loss"], 2), "Packet loss")
    lines += [
        "",
        "| System | Packet loss | Run | Video on time, first half | second half | IP reassembly failures "
        "| Peak reassembly memory |",
        "|---|---|---|---|---|---|---|",
    ]
    for r in b1["runs"]:
        if r["condition"] != "downlink" or r["system"] != "legacy":
            continue
        v, kern = r["video"], r["receiver_kernel"]
        peak = (r.get("receiver_frag_memory") or {}).get("max_bytes")
        peak_s = "n/a" if peak is None else f"{peak / 2**20:.2f} MiB"
        lines.append(
            f"| {LABELS['legacy']} | {r['loss_percent']:g} % | {r['repeat']} | "
            f"{pct(v['delivery_ratio_first_half'])} | {pct(v['delivery_ratio_second_half'])} | "
            f"{kern['ip_reasm_fails']} | {peak_s} |"
        )
    nack = [g for k in keys for g in groups[k] if g["video"].get("nack_totals") and g["loss_percent"] > 0]
    if nack:
        lines += [
            "",
            "| System | Packet loss | NACKs sent | Packets requested (with repeats) | Received or rebuilt "
            "after a request | Given up | Frames superseded | Frames expired "
            "| p99 frame latency, median (range) |",
            "|---|---|---|---|---|---|---|---|---|",
        ]
        for g in nack:
            n, a = g["video"]["nack_totals"], g["video"].get("assembler_totals") or {}
            lines.append(
                f"| {LABELS[g['system']]} | {g['loss_percent']:g} % | {n['nacks_sent']} | {n['requested']} | "
                f"{n['recovered']} | {n['abandoned']} | {a.get('superseded', 'n/a')} | "
                f"{a.get('expired', 'n/a')} | {rng(g['video']['latency_ms_p99_per_run'], 1, unit=' ms')} |"
            )
    return "\n".join(lines)


def table_b1_hard(b1: dict[str, Any]) -> str | None:
    groups = b1_groups(b1, "both-rtt50")
    keys = [k for k in B1_ORDER if k in groups]
    if not keys:
        return None
    lines = ["Video frames on time, pooled, with the Wilson 95 % interval:", ""]
    lines += _b1_grid(groups, keys, lambda g: ci(g["video"]["delivery_ratio"]), "Loss per direction")
    lines += ["", "Per run, median (range):", ""]
    lines += _b1_grid(
        groups, keys, lambda g: rng_pct(g["video"]["delivery_ratio_per_run"]), "Loss per direction"
    )
    lines += [
        "",
        "| System | Loss per direction | Runs | Frames | Measured loss, receiver to server / server to "
        "receiver | p50 / p99 frame latency (median of runs) | Frames superseded / expired | Audio lost |",
        "|---|---|---|---|---|---|---|---|",
    ]
    runs = [r for r in b1["runs"] if r["condition"] == "both-rtt50"]
    for k in keys:
        for g in groups[k]:
            mine = [r for r in runs if r["system"] == k and r["loss_percent"] == g["loss_percent"]]
            up = spread_of(r["relay"]["measured_loss_upstream"] for r in mine if r.get("relay"))
            down = spread_of(r["relay"]["measured_loss_downstream"] for r in mine if r.get("relay"))
            a = g["video"].get("assembler_totals") or {}
            lines.append(
                f"| {LABELS[k]} | {g['loss_percent']:g} % | {g['runs']} | {g['video']['frames_sent']} | "
                f"{rng_pct(up, 2)} / {rng_pct(down, 2)} | "
                f"{_num(g['video']['latency_ms_p50_per_run']['median'], 1)} / "
                f"{_num(g['video']['latency_ms_p99_per_run']['median'], 1)} ms | "
                f"{a.get('superseded', 'n/a')} / {a.get('expired', 'n/a')} | "
                f"{ci(g['audio']['residual_loss'], 2)} |"
            )
    return "\n".join(lines)


def table_b1_long(b1: dict[str, Any]) -> str | None:
    runs = [r for r in b1["runs"] if r["condition"] == "downlink-long"]
    if not runs:
        return None
    lines = []
    for r in runs:
        frag = r.get("receiver_frag_memory") or {}
        bins = r["video"]["delivery_by_30s"]
        lines += [
            f"{LABELS[r['system']]} at {r['loss_percent']:g} % loss, {r['seconds']} s run "
            f"(repeat {r['repeat']}): {ci_counts(r['video']['frames_on_time'], r['video']['frames_sent'])} "
            f"of video frames on time and {pct(r['audio']['residual_loss'], 1)} of audio lost over the "
            f"whole run; reassembly memory first reached 90 % of its "
            f"{_num((frag.get('ipfrag_high_thresh') or 0) / 2**20, 0)} MiB limit after "
            f"{frag.get('first_near_limit_s', 'n/a')} s.",
            "",
            "| Send time | " + " | ".join(f"{b['from_s']}-{b['from_s'] + 30} s" for b in bins) + " |",
            "|---" * (len(bins) + 1) + "|",
            "| Video frames on time | "
            + " | ".join(pct(b["on_time"] / b["frames"] if b["frames"] else None, 0) for b in bins)
            + " |",
            "",
        ]
    return "\n".join(lines).rstrip()


def spread_of(values: Any) -> dict[str, Any]:
    v = [float(x) for x in values if x is not None]
    if not v:
        return {"n": 0, "median": None, "min": None, "max": None}
    v.sort()
    mid = len(v) // 2
    med = v[mid] if len(v) % 2 else (v[mid - 1] + v[mid]) / 2
    return {"n": len(v), "median": med, "min": v[0], "max": v[-1]}


def ci_counts(k: int, n: int) -> str:
    return f"{k}/{n} ({pct(k / n if n else None)})"


def summary_b1(b1: dict[str, Any]) -> str:
    groups = b1_groups(b1, "downlink")
    keys = [k for k in B1_ORDER if k in groups]
    by = {(k, g["loss_percent"]): g for k in keys for g in groups[k]}
    losses = sorted({loss for _, loss in by if loss > 0})
    lines = [
        "| Random packet loss | " + " | ".join(LABELS[k] for k in keys) + " |",
        "|---" * (len(keys) + 1) + "|",
    ]
    for loss in losses:
        cells = [
            pct(by[(k, loss)]["video"]["delivery_ratio"]["p"]) if (k, loss) in by else "n/a" for k in keys
        ]
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " |")
    return "\n".join(lines)


def summary_b1_hard(b1: dict[str, Any]) -> str | None:
    groups = b1_groups(b1, "both-rtt50")
    keys = [k for k in B1_ORDER if k in groups]
    if not keys:
        return None
    by = {(k, g["loss_percent"]): g for k in keys for g in groups[k]}
    losses = sorted({loss for _, loss in by if loss > 0})
    lines = [
        "| Loss in each direction, +50 ms round trip | " + " | ".join(LABELS[k] for k in keys) + " |",
        "|---" * (len(keys) + 1) + "|",
    ]
    for loss in losses:
        cells = [ci(by[(k, loss)]["video"]["delivery_ratio"]) if (k, loss) in by else "n/a" for k in keys]
        lines.append(f"| {loss:g} % | " + " | ".join(cells) + " |")
    return "\n".join(lines)


# --- B2 ------------------------------------------------------------------------------


def b2_index(b2: dict[str, Any]) -> dict[tuple[str, int], dict[str, Any]]:
    return {(g["system"], g["participants"]): g for g in b2["groups"]}


def plot_b2(b2: dict[str, Any], theme: Theme) -> None:
    idx = b2_index(b2)
    sizes = sorted({n for _, n in idx})
    keys = [k for k in ("legacy", "v2") if any((k, n) in idx for n in sizes)]
    groups = [f"{n} parties" for n in sizes]
    runs = max(g["runs"] for g in idx.values())

    def series(field: str, scale: float = 1.0) -> tuple[dict[str, list[float | None]], Any]:
        med: dict[str, list[float | None]] = {}
        spans: dict[str, list[tuple[float, float] | None]] = {}
        for k in keys:
            med[k], spans[k] = [], []
            for n in sizes:
                s = idx[(k, n)][field] if (k, n) in idx else None
                if s is None or s.get("median") is None:
                    med[k].append(None)
                    spans[k].append(None)
                else:
                    med[k].append(scale * s["median"])
                    spans[k].append((scale * s["min"], scale * s["max"]))
        return med, spans

    charts = [
        (
            "b2_audio_latency",
            "Capture-to-playout audio latency",
            f"Median over all receivers; everybody talks and sends video; median of {runs} runs, bars: range",
            "capture_to_playout_ms_p50",
            1.0,
            lambda v: f"{v:.0f}",
            "Latency (ms)",
        ),
        (
            "b2_downstream_audio",
            "Downstream audio bandwidth per receiver",
            f"IP-level bit rate into each receiver; v2 includes its audio FEC (k=2); median of {runs} runs",
            "downstream_audio_kbit_per_s_mean",
            1.0,
            lambda v: f"{v:.0f}",
            "kbit/s",
        ),
        (
            "b2_video_shown",
            "Video frames that reached the receiver application",
            f"Share of the other participants' frames that the receivers processed; median of {runs} runs",
            "video_delivery_ratio",
            100.0,
            lambda v: f"{v:.0f}%",
            "",
        ),
    ]
    for name, title, subtitle, field, scale, fmt, ylabel in charts:
        values, spans = series(field, scale)
        fig, (ax,) = new_chart(theme, title, subtitle)
        legend(fig, theme, keys, kind="bar")
        top = max(
            max(v for vs in values.values() for v in vs if v is not None),
            max(s[1] for ss in spans.values() for s in ss if s is not None),
        )
        ymax = 105 if name == "b2_video_shown" else _nice_ceiling(top * 1.18)
        grouped_columns(fig, ax, theme, groups, keys, values, fmt, ymax, spans)
        if name == "b2_video_shown":
            percent_axis(ax)
        ax.set_ylabel(ylabel)
        save(fig, name, theme)


def table_b2(b2: dict[str, Any]) -> str:
    lines = [
        "| System | Parties | Runs | Capture-to-playout p50 (ms) | p95 (ms) | Rise over the window (ms) "
        "| Worst talker's audio played | Video frames processed | Downstream audio per receiver (kbit/s) |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for (system, n), g in sorted(b2_index(b2).items(), key=lambda kv: (kv[0][0] != "legacy", kv[0][1])):
        lines.append(
            f"| {LABELS[system]} | {n} | {g['runs']} | {rng(g['capture_to_playout_ms_p50'], 0)} | "
            f"{rng(g['capture_to_playout_ms_p95'], 0)} | {rng(g['capture_to_playout_growth_ms'], 0)} | "
            f"{rng_pct(g['audio_delivery_min_pair'])} | {rng_pct(g['video_delivery_ratio'])} | "
            f"{rng(g['downstream_audio_kbit_per_s_mean'], 0)} |"
        )
    return "\n".join(lines)


def summary_b2(b2: dict[str, Any]) -> str:
    idx = b2_index(b2)
    cols = sorted(idx, key=lambda k: (k[1], k[0] != "legacy"))
    rows: list[tuple[str, Callable[[dict[str, Any]], str]]] = [
        ("Capture-to-playout latency, median", lambda g: rng(g["capture_to_playout_ms_p50"], 0, unit=" ms")),
        ("Audio played, worst talker", lambda g: rng_pct(g["audio_delivery_min_pair"])),
        (
            "Downstream audio per receiver",
            lambda g: rng(g["downstream_audio_kbit_per_s_mean"], 0, unit=" kbit/s"),
        ),
        ("Video frames processed", lambda g: rng_pct(g["video_delivery_ratio"])),
    ]
    head = "| | " + " | ".join(f"{n} parties, {LABELS[s]}" for s, n in cols) + " |"
    lines = [head, "|---" * (len(cols) + 1) + "|"]
    for name, get in rows:
        lines.append(f"| {name} | " + " | ".join(get(idx[c]) for c in cols) + " |")
    return "\n".join(lines)


# --- B3 ------------------------------------------------------------------------------

B3_ORDER = ["legacy", "v2-gso", "v2-nogso", "v2-nobatch"]
B3_CHART_LABELS = {
    "legacy": "original",
    "v2-gso": "v2 default",
    "v2-nogso": "v2 without GSO",
    "v2-nobatch": "v2, 1 system call per datagram",
}


def plot_b3(b3: dict[str, Any], theme: Theme, name: str, label: str) -> None:
    keys = [k for k in B3_ORDER if k in b3["systems"]]
    repeats = b3["setup"].get("repeats")
    metrics = [
        (
            f"{name}_server_cpu",
            "Relay capacity: server CPU",
            f"Server process CPU / wall time (1.0 = its one pinned CPU); {label}; median and range of "
            f"{repeats} runs",
            "server_cpu_utilisation",
            (0, 1.1),
            "linear",
        ),
        (
            f"{name}_delivery",
            "Relay capacity: frame delivery",
            f"Rooms of 4, 15 fps per participant; {label}; the pass limit is 99 %",
            "delivery_ratio",
            (0.8, 1.005),
            "linear",
        ),
        (
            f"{name}_p99_latency",
            "Relay capacity: 99th-percentile frame latency",
            f"Send to last byte received; {label}; the pass limit is 100 ms",
            "p99_latency_ms",
            (0.5, 2000),
            "log",
        ),
    ]
    for out, title, subtitle, field, ylim, scale in metrics:
        fig, (ax,) = new_chart(theme, title, subtitle)
        legend(fig, theme, keys, labels=B3_CHART_LABELS)
        xs_all = [p["participants"] for k in keys for p in b3["systems"][k]["sweep"]]
        for k in keys:
            sweep = b3["systems"][k]["sweep"]
            xs = [p["participants"] for p in sweep]
            meds = [p[field]["median"] for p in sweep]
            lows = [p[field]["min"] for p in sweep]
            highs = [p[field]["max"] for p in sweep]
            if len(xs) > 1:
                ax.fill_between(xs, lows, highs, color=theme.series[k], alpha=0.18, linewidth=0, zorder=1)
            plot_line(ax, theme, k, xs, meds)
        ax.set_yscale(scale)
        if field == "delivery_ratio":  # from the next 5 % step below the lowest run, at most 95 %
            lowest = min(p[field]["min"] for k in keys for p in b3["systems"][k]["sweep"])
            ylim = (min(0.95, max(0.0, math.floor(lowest * 20) / 20)), 1.005)
        ax.set_ylim(*ylim)
        ax.set_xlim(min(xs_all) - 16, max(xs_all) + 16)
        ax.set_xlabel("Participants (rooms of 4)")
        if field == "delivery_ratio":
            ax.yaxis.set_major_locator(MultipleLocator(0.05))
            ax.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{100 * v:.0f} %"))
            ax.axhline(0.99, color=theme.muted, linewidth=px(1), linestyle=(0, (4, 3)), zorder=2)
        if field == "p99_latency_ms":
            ax.yaxis.set_major_locator(LogLocator(base=10))
            ax.yaxis.set_minor_locator(NullLocator())
            ax.yaxis.set_minor_formatter(NullFormatter())
            ax.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g} ms"))
            ax.axhline(100, color=theme.muted, linewidth=px(1), linestyle=(0, (4, 3)), zorder=2)
        if field == "server_cpu_utilisation":
            ax.yaxis.set_major_locator(MultipleLocator(0.25))
        save(fig, out, theme)


def _capacity(s: dict[str, Any]) -> str:
    caps = "/".join(str(c) for c in s["capacity_by_repeat"])
    if all(f is None for f in s["first_failing_by_repeat"]):
        sizes = {p["participants"] for p in s["sweep"]}
        return f"{caps} ({'the only size run' if len(sizes) == 1 else 'no size run failed'})"
    fails = "/".join("none" if f is None else str(f) for f in s["first_failing_by_repeat"])
    return f"{caps} (first failing: {fails})"


def table_b3(variants: list[tuple[str, dict[str, Any]]]) -> str:
    lines = [
        "| Loopback | System | Server flags | Largest size that passed, per repeat | Server CPU-s per "
        "participant-minute at 16 participants | Server CPU at 16 participants |",
        "|---|---|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            fixed = s.get("fixed") or {}
            flags = s.get("server_args") or ("(defaults)" if k.startswith("v2") else "")
            lines.append(
                f"| {label} | {LABELS[k]} | {('`' + flags + '`') if flags.startswith('-') else flags} | "
                f"{_capacity(s)} | {rng(fixed.get('server_cpu_seconds_per_participant_minute') or {}, 3)} | "
                f"{rng(fixed.get('server_cpu_utilisation') or {}, 3)} |"
            )
    lines += [
        "",
        "| Loopback | System | Participants | Runs passed | Frame delivery, median (lowest) "
        "| p99 latency, median (highest) | Server CPU | Softirq share of the server's core "
        "| Datagrams per read call | per send call | per GSO message |",
        "|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            for p in s["sweep"]:
                d, lat = p["delivery_ratio"], p["p99_latency_ms"]
                lines.append(
                    f"| {label} | {LABELS[k]} | {p['participants']} | {p['passes']}/{p['runs']} | "
                    f"{pct(d['median'], 2)} ({pct(d['min'], 2)}) | {_num(lat['median'], 1)} "
                    f"({_num(lat['max'], 1)}) ms | {rng(p['server_cpu_utilisation'], 2)} | "
                    f"{rng_pct(p['server_core_softirq_share'], 0)} | "
                    f"{rng(p['datagrams_per_read_call'], 1)} | "
                    f"{rng(p['video_datagrams_per_send_call'], 1)} | "
                    f"{rng(p['video_datagrams_per_message'], 1)} |"
                )
    return "\n".join(lines)


def summary_b3(variants: list[tuple[str, dict[str, Any]]]) -> str:
    lines = [
        "| Loopback | System | Largest size that passed, per repeat | Server CPU at 128 participants "
        "| Server CPU-s per participant-minute (16 participants) |",
        "|---|---|---|---|---|",
    ]
    for label, b3 in variants:
        for k in B3_ORDER:
            s = b3["systems"].get(k)
            if not s:
                continue
            at128 = next((p for p in s["sweep"] if p["participants"] == 128), None)
            fixed = s.get("fixed") or {}
            lines.append(
                f"| {label} | {LABELS[k]} | {_capacity(s)} | "
                f"{rng(at128['server_cpu_utilisation'], 2) if at128 else 'not run'} | "
                f"{rng(fixed.get('server_cpu_seconds_per_participant_minute') or {}, 3)} |"
            )
    return "\n".join(lines)


# --- CPU profiles ----------------------------------------------------------------------

# Profiles of the v2 server at 128 participants (bench/profile_b3.sh), as summarised by
# bench/analyze_pprof.py into pprof_<name>.json.
PROFILES = [
    ("v2-unbatched", "`-udp-batch=1`"),
    ("v2-sendmmsg", "`-udp-gso=false`"),
    ("v2-gso", "defaults (batching + GSO)"),
]
PROFILE_PATHS = [
    "forwarding video (sending the copies)",
    "reading packets",
    "audio mixers (20 ms ticks)",
    "mixed-stream sends",
    "all system calls (Syscall6)",
]


def table_profiles() -> str | None:
    lines = [
        "| Server | Server CPU | p99 latency | Forwarding | Reading | Audio mixers | of which mixer sends"
        " | All system calls | Datagrams per read call | Copies per send call | Datagrams per message"
        " | Built from | Files |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for name, label in PROFILES:
        path = RESULTS / f"pprof_{name}.json"
        if not path.exists():
            continue
        prof = json.loads(path.read_text())
        load, shares, c = prof["load"], prof["share_of_profile_percent"], prof["server_counters"] or {}
        build = prof["environment"].get("build") or {}
        cells = [f"{shares[k]:.1f} %" for k in PROFILE_PATHS]
        cells += [
            _num(c.get(k), 1)
            for k in (
                "datagrams_per_read_call",
                "video_datagrams_per_send_call",
                "video_datagrams_per_message",
            )
        ]
        lines.append(
            f"| {label} | {load['server_cpu_utilisation']:.3f} | {load['latency_ms']['p99']:.1f} ms | "
            + " | ".join(cells)
            + f" | `{build.get('git_commit', 'n/a')}` | `pprof_{name}.{{json,txt,pb.gz}}` |"
        )
    return "\n".join(lines) if len(lines) > 2 else None


# --- micro ---------------------------------------------------------------------------


def table_micro(micro: dict[str, Any]) -> str:
    runs = sorted({b["runs"] for b in micro["benchmarks"]})
    lines = [
        f"| Benchmark | ns/op, median ({'/'.join(map(str, runs))} runs) | range | MB/s | B/op | allocs/op |",
        "|---|---|---|---|---|---|",
    ]
    for b in micro["benchmarks"]:
        m, lo, hi = b["median"], b["min"], b["max"]
        mbs = f"{m['MB/s']:.0f}" if "MB/s" in m else ""
        extra = f" ({m['packets/op']:.2f} packets/op)" if "packets/op" in m else ""
        lines.append(
            f"| `{b['name']}`{extra} | {m['ns/op']:.1f} | {lo['ns/op']:.1f}-{hi['ns/op']:.1f} | {mbs} | "
            f"{m.get('B/op', 0):.0f} | {m.get('allocs/op', 0):.0f} |"
        )
    return "\n".join(lines)


CHURN_LABELS = {
    "keepalives": "keepalives only (about 204/s)",
    "keepalives+media_state": "+ 20 members toggling media_state 50 times/s",
    "keepalives+media_state+rebinds": "+ media_state toggles + 4 members rebinding every 100 ms",
    "keepalives+register_flood": "+ one member sending 1500 REGISTERs/s",
    "keepalives+media_state+rebinds_all_served": "as the third row, per-member rebind limit raised to 1000/s",
}


def _ms(v: float) -> str:
    return f"{v:,.0f}" if v >= 100 else f"{v:.3g}"


def _churn_cell(summary: dict[str, Any] | None, key: str) -> str:
    if summary is None:
        return "not measured"
    s = summary["delivered"] if key == "delivered" else summary["latency_ms"][key]
    fmt = (lambda v: f"{v:.0f}") if key == "delivered" else _ms
    if fmt(s["min"]) == fmt(s["max"]):
        return fmt(s["median"])
    return f"{fmt(s['median'])} ({fmt(s['min'])}-{fmt(s['max'])})"


def table_churn(before: dict[str, Any] | None, after: dict[str, Any]) -> str:
    """Forwarding under control-plane churn (bench/run_churn.sh), before and after the fix."""
    old = (before or {}).get("summary", {})
    runs = sorted({s["runs"] for s in after["summary"].values()} | {s["runs"] for s in old.values()})
    lines = [
        f"Median (min-max) of {'/'.join(map(str, runs))} runs of 1000 packets each.",
        "",
        "| Control-plane load besides the stream | Delivered, before | after | p50 ms, before | after "
        "| p99 ms, before | after |",
        "|---|---|---|---|---|---|---|",
    ]
    for name, a in after["summary"].items():
        b = old.get(name)
        cells = [f"{_churn_cell(b, k)} | {_churn_cell(a, k)}" for k in ("delivered", "p50", "p99")]
        lines.append(f"| {CHURN_LABELS.get(name, name)} | " + " | ".join(cells) + " |")
    return "\n".join(lines)


JITTER_MEASURES = [
    ("longest_unplayed_ms", "longest silence, ms"),
    ("gaps_over_60ms", "gaps > 60 ms"),
    ("late", "late frames"),
    ("latency_last_10s_median_ms", "median latency in the last 10 s, ms"),
]


def _whole(v: float | None) -> str:
    return "n/a" if v is None else f"{v:.0f}" if float(v).is_integer() else f"{v:.1f}"


def table_jitter(before: dict[str, Any], after: dict[str, Any]) -> str:
    """Jitter-buffer simulations (bench/run_jitter.sh): each cell is before -> after."""
    out = []
    for side, title in (("server", "Server mixer"), ("client", "Client")):
        lines = [
            f"**{title}**, before -> after:",
            "",
            "| Scenario | " + " | ".join(label for _, label in JITTER_MEASURES) + " |",
            "|---|" + "---|" * len(JITTER_MEASURES),
        ]
        for name, a in after["summary"][side].items():
            b = before["summary"][side].get(name, {})
            cells = [f"{_whole(b.get(k))} -> {_whole(a.get(k))}" for k, _ in JITTER_MEASURES]
            lines.append(f"| `{name}` | " + " | ".join(cells) + " |")
        out.append("\n".join(lines))
    return "\n\n".join(out)


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

        def fill(m: re.Match[str], body: str = body) -> str:
            return m.group(1) + body + "\n" + m.group(2)

        text = pattern.sub(fill, text)
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
    micro = load("micro.json")
    for theme in (LIGHT, DARK):
        if b1:
            plot_b1(b1, theme, "downlink", "b1_video_delivery")
            plot_b1(b1, theme, "both-rtt50", "b1_video_delivery_rtt50")
            plot_b1_audio(b1, theme)
        if b2:
            plot_b2(b2, theme)
        if variants:
            plot_b3(variants[0][1], theme, "b3", variants[0][0])
    if b1:
        blocks["b1"] = table_b1(b1)
        blocks["summary_b1"] = summary_b1(b1)
        for key, body in (
            ("b1_hard", table_b1_hard(b1)),
            ("summary_b1_hard", summary_b1_hard(b1)),
            ("b1_long", table_b1_long(b1)),
        ):
            if body:
                blocks[key] = body
    if b2:
        blocks["b2"] = table_b2(b2)
        blocks["summary_b2"] = summary_b2(b2)
    if variants:
        blocks["b3"] = table_b3(variants)
        blocks["summary_b3"] = summary_b3(variants)
    profiles = table_profiles()
    if profiles:
        blocks["b3_profile"] = profiles
    if micro:
        blocks["micro"] = table_micro(micro)
    churn_after = load("churn_after.json")
    if churn_after:
        blocks["churn"] = table_churn(load("churn_before.json"), churn_after)
    jitter_before, jitter_after = load("jitter_before.json"), load("jitter_after.json")
    if jitter_before and jitter_after:
        blocks["jitter"] = table_jitter(jitter_before, jitter_after)
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
