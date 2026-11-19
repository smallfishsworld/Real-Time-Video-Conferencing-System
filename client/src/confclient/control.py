"""Asyncio client for the TCP control plane."""

from __future__ import annotations

import asyncio
import contextlib
import itertools
from typing import Any

from .protocol import CONTROL_VERSION, ProtocolError, encode_frame, read_frame

EVENT_TYPES = frozenset({"roster", "mode", "chat", "room_closed"})


class ServerError(Exception):
    """The server answered a request with an `error` message."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


class ControlClient:
    """One control connection. Requests carry a `seq` and `request()` waits for the
    reply with the same `seq`; events (roster, mode, chat, room_closed) are put on
    `events`."""

    def __init__(self, host: str, port: int, ping_interval: float = 15.0) -> None:
        self.host = host
        self.port = port
        self.ping_interval = ping_interval
        self.events: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self.welcome: dict[str, Any] = {}
        self._reader: asyncio.StreamReader | None = None
        self._writer: asyncio.StreamWriter | None = None
        self._pending: dict[int, asyncio.Future[dict[str, Any]]] = {}
        self._seq = itertools.count(1)
        self._tasks: list[asyncio.Task[None]] = []
        self.closed = asyncio.Event()

    async def connect(self, name: str, timeout: float = 5.0) -> dict[str, Any]:
        self._reader, self._writer = await asyncio.wait_for(
            asyncio.open_connection(self.host, self.port), timeout
        )
        self._tasks.append(asyncio.create_task(self._read_loop(), name="control-reader"))
        self.welcome = await self.request(
            {"type": "hello", "name": name, "version": CONTROL_VERSION}, timeout
        )
        if self.ping_interval > 0:
            self._tasks.append(asyncio.create_task(self._ping_loop(), name="control-ping"))
        return self.welcome

    async def send(self, msg: dict[str, Any]) -> None:
        if self._writer is None or self.closed.is_set():
            raise ConnectionError("control connection is closed")
        self._writer.write(encode_frame(msg))
        await self._writer.drain()

    async def request(self, msg: dict[str, Any], timeout: float = 5.0) -> dict[str, Any]:
        seq = next(self._seq)
        fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
        self._pending[seq] = fut
        try:
            await self.send({**msg, "seq": seq})
            reply = await asyncio.wait_for(fut, timeout)
        finally:
            self._pending.pop(seq, None)
        if reply.get("type") == "error":
            raise ServerError(str(reply.get("code")), str(reply.get("message")))
        return reply

    async def close(self) -> None:
        for task in self._tasks:
            task.cancel()
        for task in self._tasks:
            with contextlib.suppress(asyncio.CancelledError, ConnectionError, OSError):
                await task
        self._tasks.clear()
        if self._writer is not None:
            self._writer.close()
            with contextlib.suppress(ConnectionError, OSError):
                await self._writer.wait_closed()
        self._fail_pending()
        self.closed.set()

    def _fail_pending(self) -> None:
        for fut in self._pending.values():
            if not fut.done():
                fut.set_exception(ConnectionError("control connection closed"))

    async def _read_loop(self) -> None:
        assert self._reader is not None
        try:
            while True:
                msg = await read_frame(self._reader)
                seq = msg.get("seq")
                if isinstance(seq, int) and seq in self._pending and not self._pending[seq].done():
                    self._pending[seq].set_result(msg)
                if msg.get("type") in EVENT_TYPES:
                    self.events.put_nowait(msg)
        except (asyncio.IncompleteReadError, ConnectionError, OSError, ProtocolError):
            pass
        finally:
            self._fail_pending()
            self.closed.set()
            self.events.put_nowait({"type": "disconnected"})

    async def _ping_loop(self) -> None:
        while not self.closed.is_set():
            await asyncio.sleep(self.ping_interval)
            with contextlib.suppress(ConnectionError, OSError, asyncio.TimeoutError, ServerError):
                await self.request({"type": "ping"}, timeout=self.ping_interval)
