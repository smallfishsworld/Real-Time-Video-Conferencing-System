"""MediaSession against an in-process fake media server: registration, P2P sending,
source and SSRC filtering, the switch grace period, FEC repair and the send cap."""

from __future__ import annotations

import asyncio
import hashlib
import itertools
import secrets
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from typing import Any

from confclient.assembler import VideoFrame
from confclient.jitter import AudioPlayout
from confclient.media import MediaConfig, MediaSession
from confclient.packetizer import AudioPacketizer, VideoPacketizer
from confclient.protocol import (
    PT_AUDIO,
    PT_FEC,
    TYPE_REGISTER,
    RtpHeader,
    build_nack,
    build_register,
    build_register_ack,
    fragment_count,
    is_rtp,
    parse_register,
    parse_rtp,
    seq_diff,
)
from confclient.testpattern import TestPattern, encode_jpeg

from .conftest import FakeClock, until

JPEG = encode_jpeg(TestPattern(240, 240).frame(0))


class FakeServer(asyncio.DatagramProtocol):
    """Answers REGISTER like the real server (a challenge for a new address, the
    binding once the nonce comes back from it) and records everything else."""

    def __init__(self) -> None:
        self.transport: asyncio.DatagramTransport | None = None
        self.received: list[tuple[bytes, tuple[str, int]]] = []
        self.registered: dict[bytes, tuple[str, int]] = {}
        self.challenges = 0

    def connection_made(self, transport: asyncio.BaseTransport) -> None:
        assert isinstance(transport, asyncio.DatagramTransport)
        self.transport = transport

    def datagram_received(self, data: bytes, addr: tuple[Any, ...]) -> None:
        src = (str(addr[0]), int(addr[1]))
        if data and data[0] == TYPE_REGISTER:
            txid, token, echoed = parse_register(data)
            assert self.transport is not None
            nonce = hashlib.sha256(token + repr(src).encode()).digest()[:8]
            if self.registered.get(token) != src and echoed != nonce:
                self.challenges += 1
                ack = build_register_ack(txid, src[0], src[1], challenge=True, nonce=nonce)
                self.transport.sendto(ack, src)
                return
            self.registered[token] = src
            self.transport.sendto(build_register_ack(txid, src[0], src[1]), src)
            return
        self.received.append((data, src))

    def send(self, pkt: bytes, addr: tuple[str, int]) -> None:
        assert self.transport is not None
        self.transport.sendto(pkt, addr)

    @property
    def port(self) -> int:
        assert self.transport is not None
        return int(self.transport.get_extra_info("sockname")[1])


def _joined(pid: int, port: int) -> dict[str, Any]:
    return {
        "type": "joined",
        "room_id": 1,
        "participant_id": pid,
        "audio_ssrc": 1000 + pid,
        "video_ssrc": 2000 + pid,
        "mixer_ssrc": 3000 + pid,
        "token": secrets.token_hex(16),
        "media_port": port,
    }


def _roster(*joined: dict[str, Any]) -> dict[str, Any]:
    return {
        "type": "roster",
        "room_id": 1,
        "participants": [
            {
                "participant_id": j["participant_id"],
                "name": f"p{j['participant_id']}",
                "audio_ssrc": j["audio_ssrc"],
                "video_ssrc": j["video_ssrc"],
                "video": True,
                "audio": True,
                "registered": True,
            }
            for j in joined
        ],
    }


class Peer:
    def __init__(self, server: FakeServer, pid: int, **kw: Any) -> None:
        self.joined = _joined(pid, server.port)
        self.frames: list[tuple[int, VideoFrame]] = []
        self.playout = AudioPlayout(delay_frames=1)
        self.session = MediaSession(
            "127.0.0.1", self.joined, self.playout, lambda p, f: self.frames.append((p, f)), **kw
        )


@asynccontextmanager
async def fake_server() -> AsyncIterator[FakeServer]:
    loop = asyncio.get_running_loop()
    transport, server = await loop.create_datagram_endpoint(FakeServer, local_addr=("127.0.0.1", 0))
    try:
        yield server
    finally:
        transport.close()


def _run(test: Callable[[], Any]) -> None:
    asyncio.run(asyncio.wait_for(test(), 20))


def _p2p_mode(peer: Peer) -> dict[str, Any]:
    host, port = peer.session.local_addr
    return {
        "type": "mode",
        "room_id": 1,
        "mode": "p2p",
        "peer": {
            "participant_id": peer.joined["participant_id"],
            "audio_ssrc": peer.joined["audio_ssrc"],
            "video_ssrc": peer.joined["video_ssrc"],
            "addr": f"{host}:{port}",
        },
    }


def test_register_then_p2p_media_bypasses_the_server() -> None:
    async def scenario() -> None:
        async with fake_server() as server:
            a, b = Peer(server, 1), Peer(server, 2)
            observed = await a.session.start()
            await b.session.start()
            assert observed == a.session.local_addr  # the server saw our real address
            assert len(server.registered) == 2
            # Each socket proved its address by echoing one challenge.
            assert server.challenges == 2
            assert a.session.stats.challenges == b.session.stats.challenges == 1

            assert a.session.send_video(JPEG, 0.0) is None  # nobody else in the room yet
            for p in (a, b):
                p.session.update_roster(_roster(a.joined, b.joined))
            a.session.update_mode(_p2p_mode(b))
            b.session.update_mode(_p2p_mode(a))
            assert a.session.targets == [b.session.local_addr]

            assert a.session.send_video(JPEG, 0.1) is not None
            assert a.session.send_audio(bytes(640), 0.1)
            await until(lambda: len(b.frames) == 1 and bool(b.playout.stream_stats()))
            pid, frame = b.frames[0]
            assert pid == 1
            assert frame.data == JPEG
            assert set(b.playout.stream_stats()) == {a.joined["audio_ssrc"]}
            assert b.session.stats.packets_by_source["peer"] > 0
            assert [d for d, _ in server.received] == []  # nothing went through the server
            await a.session.close()
            await b.session.close()

    _run(scenario)


def test_binding_lost_later_is_restored_by_answering_the_challenge() -> None:
    async def scenario() -> None:
        async with fake_server() as server:
            a = Peer(server, 1)
            await a.session.start()
            token = bytes.fromhex(a.joined["token"])
            del server.registered[token]  # as when the server expired the binding
            # The next keepalive is challenged; the session echoes the nonce at once.
            a.session._send_raw(build_register(7, token), a.session.server)
            await until(lambda: token in server.registered)
            assert server.registered[token] == a.session.local_addr
            assert a.session.stats.challenges == 2
            await a.session.close()

    _run(scenario)


def test_rejects_unknown_sources_and_ssrcs() -> None:
    async def scenario() -> None:
        async with fake_server() as server:
            a, b = Peer(server, 1), Peer(server, 2)
            await a.session.start()
            await b.session.start()
            b.session.update_roster(_roster(a.joined, b.joined))
            stranger = VideoPacketizer(ssrc=a.joined["video_ssrc"], fec_k=0)
            _fid, pkts = stranger.packetize(JPEG, 0)
            # Right SSRC, but from an address that is neither the server nor b's peer.
            loop = asyncio.get_running_loop()
            transport, _ = await loop.create_datagram_endpoint(
                asyncio.DatagramProtocol, local_addr=("127.0.0.1", 0)
            )
            for p in pkts:
                transport.sendto(p, b.session.local_addr)
            # Right source (the server), unknown SSRC.
            rogue = AudioPacketizer(ssrc=0xDEAD, fec_k=0).packetize(bytes(640), 0)[0]
            server.send(rogue, b.session.local_addr)
            await until(lambda: b.session.stats.unknown_ssrc == 1)
            await until(lambda: b.session.stats.rejected_source == len(pkts))
            assert b.frames == []
            transport.close()
            await a.session.close()
            await b.session.close()

    _run(scenario)


def test_old_peer_is_accepted_only_during_the_switch_grace() -> None:
    async def scenario() -> None:
        async with fake_server() as server:
            clock = FakeClock()
            a = Peer(server, 1)
            b = Peer(server, 2, clock=clock, config=MediaConfig(switch_grace=2.0))
            await a.session.start()
            await b.session.start()
            for p in (a, b):
                p.session.update_roster(_roster(a.joined, b.joined))
            a.session.update_mode(_p2p_mode(b))
            b.session.update_mode(_p2p_mode(a))
            b.session.update_mode({"type": "mode", "room_id": 1, "mode": "relay"})
            assert b.session.targets == [("127.0.0.1", server.port)]

            a.session.send_video(JPEG, 0.0)  # a has not seen the switch yet
            await until(lambda: len(b.frames) == 1)
            clock.advance(2.5)
            a.session.send_video(JPEG, 0.1)
            await until(lambda: b.session.stats.rejected_source > 0)
            assert len(b.frames) == 1
            await a.session.close()
            await b.session.close()

    _run(scenario)


def test_relay_path_with_one_loss_per_fec_group() -> None:
    """The fake server relays a's packets to b, dropping one media packet of every FEC
    group; b's FEC decoder rebuilds them and every frame arrives intact."""

    async def scenario() -> None:
        async with fake_server() as server:
            a, b = Peer(server, 1), Peer(server, 2)
            await a.session.start()
            await b.session.start()
            for p in (a, b):
                p.session.update_roster(_roster(a.joined, b.joined))
            for i in range(5):
                a.session.send_video(JPEG, i / 15)
            fragments = fragment_count(len(JPEG))
            await until(lambda: len(server.received) == 5 * (fragments + -(-fragments // 4)))
            group: list[bytes] = []
            for data, _src in server.received:
                group.append(data)
                if data[1] & 0x7F == PT_FEC:
                    del group[0]  # lose the first media packet of the group
                    for pkt in group:
                        server.send(pkt, b.session.local_addr)
                    group = []
            await until(lambda: len(b.frames) == 5)
            assert all(f.data == JPEG for _, f in b.frames)
            assert b.session.stats.fec_recovered == sum(
                1 for d, _ in server.received if d[1] & 0x7F == PT_FEC
            )
            await a.session.close()
            await b.session.close()

    _run(scenario)


def test_send_cap_drops_whole_frames() -> None:
    async def scenario() -> None:
        async with fake_server() as server:
            cfg = MediaConfig(max_send_kbps=100, send_burst_bytes=len(JPEG) // 2)
            a, b = Peer(server, 1, config=cfg), Peer(server, 2)
            await a.session.start()
            a.session.update_roster(_roster(a.joined, b.joined))
            assert a.session.send_video(JPEG, 0.0) is None  # larger than the whole burst
            assert a.session.stats.video_frames_rate_limited == 1
            assert a.session.send_audio(bytes(640), 0.0)  # audio still fits
            await until(lambda: len(server.received) == 1)
            await a.session.close()

    _run(scenario)


def _rtp_of(server: FakeServer, ssrc: int, pt: int | None = None) -> list[RtpHeader]:
    out = []
    for data, _src in server.received:
        if is_rtp(data):
            header, _ = parse_rtp(data)
            if header.ssrc == ssrc and (pt is None or header.payload_type == pt):
                out.append(header)
    return out


def test_frames_refused_by_the_cap_use_no_sequence_numbers() -> None:
    """Review finding client-4: a refused frame used to be packetized first, so the
    receivers saw a sequence gap and asked for packets that were never sent."""

    async def scenario() -> None:
        async with fake_server() as server:
            clock = FakeClock()
            _packets, size = VideoPacketizer(1, 4).wire_size(len(JPEG))
            cfg = MediaConfig(max_send_kbps=100, send_burst_bytes=int(1.5 * size) + 2000)
            a, b = Peer(server, 1, config=cfg, clock=clock), Peer(server, 2)
            await a.session.start()
            a.session.update_roster(_roster(a.joined, b.joined))
            assert a.session.send_video(JPEG, 0.0) is not None
            assert a.session.send_video(JPEG, 0.1) is None  # the budget is spent
            clock.advance(10)
            assert a.session.send_video(JPEG, 10.0) is not None
            ssrc = a.joined["video_ssrc"]
            per_frame = len(VideoPacketizer(1, 4).packetize(JPEG, 0)[1])
            await until(lambda: len(_rtp_of(server, ssrc)) == 2 * per_frame)
            seqs = [h.sequence for h in _rtp_of(server, ssrc)]
            assert all(seq_diff(y, x) == 1 for x, y in itertools.pairwise(seqs))
            assert a.session.stats.video_frames_rate_limited == 1
            await a.session.close()

    _run(scenario)


def test_a_frame_larger_than_the_burst_goes_out_once_the_bucket_refills() -> None:
    """Review finding client-5: with a fixed 128 KiB burst an HD frame never fit, at any
    rate. The burst now grows to two such frames; the rate cap stays as it was."""

    async def scenario() -> None:
        async with fake_server() as server:
            clock = FakeClock()
            cfg = MediaConfig(max_send_kbps=1000, send_burst_bytes=1000)
            a, b = Peer(server, 1, config=cfg, clock=clock), Peer(server, 2)
            await a.session.start()
            a.session.update_roster(_roster(a.joined, b.joined))
            assert a.session.send_video(JPEG, 0.0) is None
            assert a.session.send_audio(bytes(640), 0.0)  # audio is not starved meanwhile
            clock.advance(2 * len(JPEG) * 8 / 1_000_000)  # time to earn the frame at 1 Mbit/s
            assert a.session.send_video(JPEG, 0.5) is not None
            assert a.session.stats.video_frames_rate_limited == 1
            await a.session.close()

    _run(scenario)


def test_audio_timestamps_count_frames_and_jump_only_over_pauses() -> None:
    """Review finding client-8: one late capture callback used to shift every later
    timestamp. The timestamp now counts frames; only a pause of more than ten frames
    (or a time with nobody to send to) moves it by the capture clock."""

    async def scenario() -> None:
        async with fake_server() as server:
            a, b = Peer(server, 1), Peer(server, 2)
            await a.session.start()
            a.session.update_roster(_roster(a.joined, b.joined))
            pcm = bytes(640)
            for t in (0.0, 0.02, 0.07, 0.075, 0.10):  # one callback 30 ms late, then a quick one
                assert a.session.send_audio(pcm, t)
            assert a.session.send_audio(pcm, 1.0)  # a 0.9 s pause: 45 frames
            a.session.update_roster(_roster(a.joined))  # alone: nothing is sent ...
            for n in range(1, 11):
                assert not a.session.send_audio(pcm, 1.0 + 0.02 * n)
            a.session.update_roster(_roster(a.joined, b.joined))
            assert a.session.send_audio(pcm, 1.22)  # ... but the timestamp kept counting
            ssrc = a.joined["audio_ssrc"]
            await until(lambda: len(_rtp_of(server, ssrc, PT_AUDIO)) == 7)
            headers = _rtp_of(server, ssrc, PT_AUDIO)
            frames = [((h.timestamp - headers[0].timestamp) & 0xFFFFFFFF) // 320 for h in headers]
            assert frames == [0, 1, 2, 3, 4, 49, 60]
            assert [h.marker for h in headers] == [True, False, False, False, False, True, True]
            await a.session.close()

    _run(scenario)


def test_nacks_stay_within_the_feedback_budget_and_registers_are_charged() -> None:
    """Review finding client-11: NACK and REGISTER packets bypassed every budget. NACKs
    now have their own (feedback_kbps, feedback_pps); a REGISTER always goes out, and
    the NACKs after it get less."""

    async def scenario() -> None:
        async with fake_server() as server:
            clock = FakeClock()
            cfg = MediaConfig(feedback_kbps=128, feedback_pps=100)  # burst: 10 packets
            a = Peer(server, 1, config=cfg, clock=clock)
            await a.session.start()  # its REGISTER and challenge echo are charged too
            sent_before = len(server.received)
            nack = build_nack(a.joined["video_ssrc"], 1234, [1, 2, 3])
            sent = sum(a.session._send_feedback(nack) for _ in range(50))
            assert sent == 10 - 2  # the burst, less the REGISTER and its echo
            assert not a.session._send_feedback(nack)
            assert a.session._send_feedback(build_register(1, bytes(16)), must=True)
            clock.advance(0.051)  # 5 packets' worth, less the REGISTER's debt
            assert sum(a.session._send_feedback(nack) for _ in range(50)) == 4
            await until(lambda: len(server.received) == sent_before + sent + 4)
            await a.session.close()

    _run(scenario)
