"""Small asyncio helpers."""

from __future__ import annotations

import asyncio
from collections.abc import Callable, Iterable
from typing import Any


async def cancel_and_wait(
    tasks: Iterable[asyncio.Future[Any]], out: Callable[[str], None] | None = None
) -> None:
    """Cancel tasks and wait until they have finished.

    Unlike suppressing CancelledError around each await, this lets a cancellation of
    the caller itself propagate, so a caller that is being cancelled does not carry on
    as if nothing had happened. Failures other than cancellation are reported through
    `out` when it is given.
    """
    tasks = list(tasks)
    for task in tasks:
        task.cancel()
    results = await asyncio.gather(*tasks, return_exceptions=True)
    for task, res in zip(tasks, results, strict=True):
        if out is not None and isinstance(res, Exception):
            name = task.get_name() if isinstance(task, asyncio.Task) else "task"
            out(f"! {name} failed: {res!r}")
