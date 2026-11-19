"""End to end: build the Go server, run it on loopback, and drive headless clients with
fake media through it.

Run with `uv run pytest -m e2e`. The Go toolchain must be on PATH, or CONFSERVER_BIN
must point to a built `confserver` binary.
"""

from __future__ import annotations

import asyncio
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np
import pytest

from confclient.app import ClientConfig, ConferenceApp, Devices
from confclient.devices import AudioSink, AudioSource, FakeCamera, FakeMicrophone, FakeSpeaker, VideoSource
from confclient.jitter import Frame
from confclient.protocol import AUDIO_CLOCK_RATE, PT_VIDEO, peek_ssrc

from .conftest import REPO, until

pytestmark = pytest.mark.e2e


@pytest.fixture(scope="session")
def server_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    prebuilt = os.environ.get("CONFSERVER_BIN")
    if prebuilt:
        return Path(prebuilt)
    go = shutil.which("go")
    if go is None:
        pytest.skip("Go toolchain not found; set CONFSERVER_BIN to a built confserver binary")
    out = tmp_path_factory.mktemp("bin") / ("confserver.exe" if os.name == "nt" else "confserver")
    subprocess.run(
        [go, "build", "-o", str(out), "./cmd/confserver"], cwd=REPO / "server", check=True, timeout=600
    )
    return out


def _free_port(kind: socket.SocketKind = socket.SOCK_STREAM) -> int:
    with socket.socket(socket.AF_INET, kind) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


@dataclass
class Server:
    control_port: int
    media_port: int
    metrics_port: int
    log: Path

    def metrics(self) -> dict[str, float]:
        url = f"http://127.0.0.1:{self.metrics_port}/metrics"
        with urllib.request.urlopen(url, timeout=5) as resp:
            text = resp.read().decode()
        out = {}
        for line in text.splitlines():
            if line and not line.startswith("#"):
                key, _, value = line.rpartition(" ")
                out[key] = float(value)
        return out


@pytest.fixture
def server(server_binary: Path, tmp_path: Path) -> Iterator[Server]:
    srv = Server(_free_port(), _free_port(socket.SOCK_DGRAM), _free_port(), tmp_path / "server.log")
    with srv.log.open("wb") as log:
        proc = subprocess.Popen(
            [
                str(server_binary),
                f"-control-addr=127.0.0.1:{srv.control_port}",
                f"-media-addr=127.0.0.1:{srv.media_port}",
                f"-metrics-addr=127.0.0.1:{srv.metrics_port}",
                "-log-level=debug",
            ],
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        try:
            deadline = time.monotonic() + 10
            while True:
                assert proc.poll() is None, srv.log.read_text()
                try:
                    socket.create_connection(("127.0.0.1", srv.control_port), timeout=1).close()
                    break
                except OSError:
                    assert time.monotonic() < deadline, "server did not start"
                    time.sleep(0.05)
            yield srv
        finally:
            proc.terminate()
            try:
                rc = proc.wait(10)
            except subprocess.TimeoutExpired:
                proc.kill()
                rc = proc.wait()
    if os.name != "nt":
        assert rc == 0, srv.log.read_text()


# In-process clients -------------------------------------------------------------------

FREQS = {"alice": 400.0, "bob": 700.0, "carol": 1100.0}  # each talker's test tone (Hz)


@dataclass
class Client:
    name: str
    app: ConferenceApp
    log: list[str]
    speakers: list[FakeSpeaker]
    played: list[Frame] = field(default_factory=list)

    @property
    def pid(self) -> int:
        assert self.app.room is not None
        return int(self.app.room["participant_id"])

    @property
    def audio_ssrc(self) -> int:
        assert self.app.room is not None
        return int(self.app.room["audio_ssrc"])

    def stats(self) -> Any:
        assert self.app.session is not None
        return self.app.session.stats

    def mode(self) -> str | None:
        return None if self.app.session is None else self.app.session.mode

    def frames_from(self, other: Client) -> int:
        s = self.app.session
        return 0 if s is None else s.stats.frames_by_participant.get(other.pid, 0)


async def _client(server: Server, name: str) -> Client:
    speakers: list[FakeSpeaker] = []

    def camera() -> VideoSource:
        return FakeCamera(240, 240, fps=10, loop_frames=10)

    def mic() -> AudioSource:
        return FakeMicrophone(freq=FREQS[name], amplitude=3000)

    def speaker() -> AudioSink:
        s = FakeSpeaker()
        speakers.append(s)
        return s

    log: list[str] = []
    app = ConferenceApp(
        ClientConfig(name=name, server="127.0.0.1", port=server.control_port),
        Devices(camera, mic, speaker),
        log.append,
    )
    client = Client(name, app, log, speakers)
    app.playout.observer = lambda frame, _t: client.played.append(frame)
    await app.start()
    return client


def _tone_levels(frames: list[Frame]) -> dict[str, float]:
    """Spectral magnitude of each talker's tone over one second of played audio."""
    x = np.concatenate(frames).astype(np.float64)
    spectrum = np.abs(np.fft.rfft(x * np.hanning(len(x))))
    hz_per_bin = AUDIO_CLOCK_RATE / len(x)
    return {name: float(spectrum[round(f / hz_per_bin)]) for name, f in FREQS.items()}


def _run(scenario: Callable[[], Any]) -> None:
    asyncio.run(asyncio.wait_for(scenario(), 90))


def test_three_party_call_video_mixed_audio_and_chat(server: Server) -> None:
    async def scenario() -> None:
        clients = [await _client(server, name) for name in FREQS]
        alice, bob, carol = clients
        room = (await alice.app.create("e2e"))["room_id"]
        await bob.app.join(room)
        await carol.app.join(room)

        # Three members: the server relays. Wait out both switch grace periods so that
        # everything counted below took the relay path.
        await until(lambda: all(c.mode() == "relay" for c in clients), timeout=10)
        await asyncio.sleep(2.5)
        before = {(c.name, o.name): c.frames_from(o) for c in clients for o in clients if o is not c}
        peer_before = {c.name: c.stats().packets_by_source["peer"] for c in clients}

        # Video: every client receives fresh frames from both others, via the server.
        await until(
            lambda: all(
                c.frames_from(o) >= before[c.name, o.name] + 10
                for c in clients
                for o in clients
                if o is not c
            ),
            timeout=15,
        )
        for c in clients:
            assert c.stats().packets_by_source["peer"] == peer_before[c.name]

        # Audio: one mixed stream that carries both other talkers and never your own.
        for c in clients:
            others = {o.audio_ssrc for o in clients if o is not c}
            assert others <= c.stats().mixed_csrcs
            assert c.audio_ssrc not in c.stats().mixed_csrcs
        for c in clients:
            c.played.clear()
        await until(lambda: all(len(c.played) >= 55 for c in clients), timeout=10)
        for c in clients:
            levels = _tone_levels(c.played[-50:])
            own = levels[c.name]
            for other in clients:
                if other is not c:
                    assert levels[other.name] > 100 * own, (c.name, levels)  # own voice 40 dB down
            assert c.speakers[0].nonsilent > 0

        # The server agrees: one relay room with three members, nothing malformed.
        metrics = await asyncio.to_thread(server.metrics)
        assert metrics['confserver_rooms{mode="relay"}'] == 1
        assert metrics["confserver_participants"] == 3
        assert metrics['confserver_media_dropped_packets_total{reason="malformed"}'] == 0
        assert metrics['confserver_media_packets_sent_total{kind="audio"}'] > 0

        # Chat: everybody sees every message, stamped by the server with name and time.
        for c in clients:
            await c.app.chat(f"hello from {c.name}")
        await until(lambda: all(len(c.app.chat_log) == 3 for c in clients))
        now_ms = time.time() * 1000
        for c in clients:
            assert sorted((m.from_name, m.text) for m in c.app.chat_log) == sorted(
                (n, f"hello from {n}") for n in FREQS
            )
            assert all(abs(m.server_time_ms - now_ms) < 60_000 for m in c.app.chat_log)
            assert any(line.endswith(f"] {c.name}: hello from {c.name}") for line in c.log)

        for c in clients:
            await c.app.leave()
            await c.app.close()

    _run(scenario)


def test_lost_video_is_resent_after_nack(server: Server) -> None:
    """Carol loses two of every ten video packets on first arrival, in pairs so that
    the XOR parity (one repair per group) cannot fill them; her NACKs must bring them
    back from the server's buffer, so she sees about as many frames as Bob."""

    async def scenario() -> None:
        clients = [await _client(server, name) for name in FREQS]
        alice, bob, carol = clients
        room = (await alice.app.create("nack"))["room_id"]
        await bob.app.join(room)
        await carol.app.join(room)
        await until(lambda: all(c.mode() == "relay" for c in clients), timeout=10)
        await asyncio.sleep(2.5)  # past the switch grace period: everything is relayed

        session = carol.app.session
        assert session is not None
        dropped: set[tuple[int, int]] = set()
        receive = session.datagram_received

        def lossy(data: bytes, addr: tuple[Any, ...]) -> None:
            if len(data) >= 12 and data[1] & 0x7F == PT_VIDEO:
                ssrc, _pt, seq = peek_ssrc(data)
                if seq % 10 in (0, 1) and (ssrc, seq) not in dropped:
                    dropped.add((ssrc, seq))
                    return
            receive(data, addr)

        session.datagram_received = lossy  # type: ignore[method-assign]
        start = {c.name: c.frames_from(alice) for c in (bob, carol)}
        await asyncio.sleep(6)
        got = {c.name: c.frames_from(alice) - start[c.name] for c in (bob, carol)}
        nack = session.nack_stats()
        assert len(dropped) > 50
        # A pair that straddles two parity groups is one loss per group, which FEC
        # repairs before any NACK is due; the rest must come back by retransmission.
        detail = (nack, session.stats.fec_recovered, len(dropped))
        assert nack.recovered + session.stats.fec_recovered >= 0.9 * len(dropped), detail
        assert nack.recovered >= 0.3 * len(dropped), detail
        assert got["carol"] >= 0.9 * got["bob"] > 0, got
        metrics = await asyncio.to_thread(server.metrics)
        assert metrics['confserver_media_packets_sent_total{kind="video_rtx"}'] >= nack.recovered
        assert metrics['confserver_media_packets_received_total{kind="nack"}'] == session.stats.nacks_sent

        for c in clients:
            await c.app.leave()
            await c.app.close()

    _run(scenario)


def test_p2p_switching_and_cancel(server: Server) -> None:
    async def scenario() -> None:
        alice, bob = await _client(server, "alice"), await _client(server, "bob")
        room = (await alice.app.create("p2p"))["room_id"]
        await bob.app.join(room)

        # Two members: direct media, and after the relay grace nothing from the server.
        await until(lambda: alice.mode() == "p2p" and bob.mode() == "p2p", timeout=10)
        await asyncio.sleep(1.5)
        server_before = {c.name: c.stats().packets_by_source["server"] for c in (alice, bob)}
        peer_before = {c.name: c.stats().packets_by_source["peer"] for c in (alice, bob)}
        frames_before = bob.frames_from(alice)
        await asyncio.sleep(1.0)
        for c in (alice, bob):
            assert c.stats().packets_by_source["server"] == server_before[c.name]
            assert c.stats().packets_by_source["peer"] > peer_before[c.name]
        assert bob.frames_from(alice) >= frames_before + 5
        metrics = await asyncio.to_thread(server.metrics)
        assert metrics['confserver_rooms{mode="p2p"}'] == 1

        # A third member switches the room to relay ...
        carol = await _client(server, "carol")
        await carol.app.join(room)
        await until(lambda: all(c.mode() == "relay" for c in (alice, bob, carol)), timeout=10)
        await until(lambda: alice.frames_from(carol) >= 5 and bob.frames_from(carol) >= 5, timeout=10)

        # ... and when it leaves, the remaining two go back to P2P.
        await carol.app.leave()
        await carol.app.close()
        await until(lambda: alice.mode() == "p2p" and bob.mode() == "p2p", timeout=10)
        peer_before = {c.name: c.stats().packets_by_source["peer"] for c in (alice, bob)}
        await until(
            lambda: all(c.stats().packets_by_source["peer"] > peer_before[c.name] + 50 for c in (alice, bob)),
            timeout=10,
        )
        assert any("media mode: peer-to-peer with alice" in line for line in bob.log)

        # The host cancels the meeting: everybody is removed.
        await alice.app.cancel()
        await asyncio.wait_for(bob.app.room_closed.wait(), 5)
        assert bob.app.room is None
        assert alice.app.room is None
        assert any("closed (cancelled)" in line for line in bob.log)
        metrics = await asyncio.to_thread(server.metrics)
        assert metrics['confserver_rooms{mode="p2p"}'] == 0
        assert metrics['confserver_rooms{mode="relay"}'] == 0
        for c in (alice, bob):
            await c.app.close()

    _run(scenario)


def test_command_line_clients(server: Server) -> None:
    """Two `python -m confclient` processes driven by --script."""
    common = [
        sys.executable,
        "-m",
        "confclient",
        f"--port={server.control_port}",
        "--fake-media",
        "--headless",
        "--width=160",
        "--height=120",
        "--fps=5",
    ]
    env = {**os.environ, "PYTHONUNBUFFERED": "1"}

    def start(*args: str) -> subprocess.Popen[str]:
        return subprocess.Popen(
            [*common, *args],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            env=env,
        )

    alice = start(
        "--name=alice", "--create=demo", "--script=sleep 4; chat hello from alice; sleep 3; status; exit"
    )
    time.sleep(1.0)
    # Bob stays until well after alice's `status` (about 7 s in), so that alice still
    # reports the peer-to-peer mode however long the interpreters take to start.
    bob = start("--name=bob", "--script=join 1; sleep 1; chat hi from bob; sleep 8; exit")
    out_a, _ = alice.communicate(timeout=60)
    out_b, _ = bob.communicate(timeout=60)
    assert alice.returncode == 0, out_a
    assert bob.returncode == 0, out_b
    assert "* created meeting 1 (demo)" in out_a
    assert "* media mode: peer-to-peer with bob" in out_a
    assert "] bob: hi from bob" in out_a
    assert "] alice: hello from alice" in out_a
    assert "mode: p2p" in out_a
    assert "* joined meeting 1 (demo)" in out_b
    assert "] alice: hello from alice" in out_b
