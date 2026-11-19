"""ControlClient and the app's event handling against an in-process fake control server:
replies, errors with and without a seq, and a server that sends garbage."""

from __future__ import annotations

import asyncio
import struct
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from typing import Any

import pytest

from confclient.app import ClientConfig, ConferenceApp, Devices
from confclient.control import ControlClient, ServerError
from confclient.protocol import encode_frame, read_frame

Handler = Callable[[asyncio.StreamReader, asyncio.StreamWriter], Awaitable[None]]

WELCOME = {"type": "welcome", "client_id": 1, "version": 1, "server_time_ms": 0, "media_port": 6100}


@asynccontextmanager
async def control_server(handler: Handler) -> AsyncIterator[int]:
    async def serve(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            await handler(reader, writer)
        except (asyncio.IncompleteReadError, ConnectionError):
            pass
        finally:
            writer.close()

    server = await asyncio.start_server(serve, "127.0.0.1", 0)
    try:
        yield int(server.sockets[0].getsockname()[1])
    finally:
        server.close()
        await server.wait_closed()


async def _send(writer: asyncio.StreamWriter, msg: dict[str, Any]) -> None:
    writer.write(encode_frame(msg))
    await writer.drain()


async def _welcome(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    hello = await read_frame(reader)
    assert hello["type"] == "hello"
    await _send(writer, {**WELCOME, "seq": hello["seq"]})


def _run(test: Callable[[], Awaitable[None]]) -> None:
    asyncio.run(asyncio.wait_for(test(), 10))


def test_an_error_without_a_seq_is_an_event() -> None:
    """An error the server sends before it has read a seq (a refused or unreadable
    frame) used to answer nothing and be dropped."""

    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await _welcome(reader, writer)
        await _send(writer, {"type": "error", "code": "rate_limited", "message": "slow down"})
        await reader.read()

    async def scenario() -> None:
        async with control_server(handler) as port:
            client = ControlClient("127.0.0.1", port, ping_interval=0)
            await client.connect("alice")
            ev = await asyncio.wait_for(client.events.get(), 5)
            assert ev == {"type": "error", "code": "rate_limited", "message": "slow down"}
            await client.close()

    _run(scenario)


def test_an_error_reply_raises_and_is_not_also_an_event() -> None:
    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await _welcome(reader, writer)
        req = await read_frame(reader)
        await _send(writer, {"type": "error", "seq": req["seq"], "code": "not_in_room", "message": "no"})
        await _send(
            writer,
            {"type": "chat", "room_id": 1, "from_id": 2, "from_name": "b", "text": "x", "server_time_ms": 0},
        )
        await reader.read()

    async def scenario() -> None:
        async with control_server(handler) as port:
            client = ControlClient("127.0.0.1", port, ping_interval=0)
            await client.connect("alice")
            with pytest.raises(ServerError) as err:
                await client.request({"type": "chat", "text": "hi"})
            assert (err.value.code, err.value.message) == ("not_in_room", "no")
            ev = await asyncio.wait_for(client.events.get(), 5)
            assert ev["type"] == "chat"  # the next event: the error was not queued
            await client.close()

    _run(scenario)


def test_a_refused_hello_raises_server_error_and_close_still_works() -> None:
    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        hello = await read_frame(reader)
        await _send(writer, {"type": "error", "seq": hello["seq"], "code": "bad_request", "message": "name"})

    async def scenario() -> None:
        async with control_server(handler) as port:
            app = ConferenceApp(ClientConfig(name="x" * 40, port=port), Devices(None, None, None), print)
            with pytest.raises(ServerError, match="bad_request"):
                await app.start()
            await app.close()  # nothing left running, nothing raised

    _run(scenario)


@pytest.mark.parametrize(
    "garbage",
    [
        struct.pack("!I", 60_000) + b"[" * 30_000 + b"]" * 30_000,  # nesting too deep
        struct.pack("!I", 0),  # a zero-length frame
        struct.pack("!I", 4) + b"null",  # no type
    ],
    ids=["deep_nesting", "zero_length", "no_type"],
)
def test_a_server_that_sends_garbage_disconnects_cleanly(garbage: bytes) -> None:
    """A RecursionError used to escape the reader task, and close() then raised it."""

    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await _welcome(reader, writer)
        writer.write(garbage)
        await writer.drain()
        await reader.read()

    async def scenario() -> None:
        async with control_server(handler) as port:
            client = ControlClient("127.0.0.1", port, ping_interval=0)
            await client.connect("alice")
            assert await asyncio.wait_for(client.events.get(), 5) == {"type": "disconnected"}
            with pytest.raises(ConnectionError):
                await client.request({"type": "list_rooms"})
            await client.close()

    _run(scenario)


def test_the_app_prints_errors_that_answer_no_request() -> None:
    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await _welcome(reader, writer)
        await _send(writer, {"type": "error", "code": "rate_limited", "message": "slow down"})
        await reader.read()

    async def scenario() -> None:
        async with control_server(handler) as port:
            log: list[str] = []
            app = ConferenceApp(ClientConfig(name="alice", port=port), Devices(None, None, None), log.append)
            await app.start()
            for _ in range(100):
                if log:
                    break
                await asyncio.sleep(0.01)
            assert log == ["! server: rate_limited: slow down"]
            await app.close()

    _run(scenario)


def test_a_request_without_a_reply_times_out() -> None:
    async def handler(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        await _welcome(reader, writer)
        await reader.read()  # never answers

    async def scenario() -> None:
        async with control_server(handler) as port:
            client = ControlClient("127.0.0.1", port, ping_interval=0)
            await client.connect("alice")
            with pytest.raises(TimeoutError):
                await client.request({"type": "list_rooms"}, timeout=0.2)
            assert not client._pending
            await client.close()

    _run(scenario)
