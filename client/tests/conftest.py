from __future__ import annotations

import asyncio
import json
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[2]
VECTORS: dict[str, list[dict[str, Any]]] = json.loads(
    (REPO / "protocol-testdata" / "vectors.json").read_text(encoding="utf-8")
)


class FakeClock:
    """A manually advanced clock for code that takes a `clock` callable."""

    def __init__(self, t: float = 1000.0) -> None:
        self.t = t

    def __call__(self) -> float:
        return self.t

    def advance(self, dt: float) -> None:
        self.t += dt


async def until(cond: Callable[[], bool], timeout: float = 5.0, interval: float = 0.02) -> None:
    """Wait until cond() is true; raise AssertionError on timeout."""
    deadline = time.monotonic() + timeout
    while not cond():
        if time.monotonic() > deadline:
            raise AssertionError(f"condition not met within {timeout} s")
        await asyncio.sleep(interval)
