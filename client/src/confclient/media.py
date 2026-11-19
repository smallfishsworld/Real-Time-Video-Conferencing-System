"""The media session of one room membership: UDP registration, sending video and
audio with a bitrate cap, and receiving, repairing and reassembling other members'
media from the relay or, in P2P mode, directly from the peer."""

from __future__ import annotations

import asyncio
import contextlib
import ipaddress
import math
import secrets
import socket
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from .aio import cancel_and_wait
from .assembler import FrameAssembler, VideoFrame
from .fec import FecDecoder
from .jitter import AudioPlayout
from .nack import NackStats, NackTracker
from .packetizer import AudioPacketizer, VideoPacketizer
from .protocol import (
    AUDIO_FRAME_SECONDS,
    AUDIO_SAMPLES_PER_FRAME,
    PT_AUDIO,
    PT_FEC,
    PT_VIDEO,
    TYPE_REGISTER_ACK,
    VIDEO_CLOCK_RATE,
    ProtocolError,
    build_nacks,
    build_register,
    is_rtp,
    parse_addr,
    parse_register_ack,
    parse_rtp,
    peek_ssrc,
    seq_diff,
)
from .ratelimit import TokenBucket

Addr = tuple[str, int]
_WIRE_OVERHEAD = 28  # IPv4 + UDP header bytes charged to the send budgets per packet
_KINDS = ("video", "video_fec", "audio", "audio_fec")
# A capture pause longer than this many frames (a muted or stalled device) moves the
# audio timestamp forward by the pause; shorter irregularities do not.
_AUDIO_GAP_FRAMES = 10


@dataclass
class MediaConfig:
    # Cap on media: video and audio packets with their FEC parity, IP/UDP headers
    # included. None sends without a cap.
    max_send_kbps: int | None = 6000
    send_burst_bytes: int = 128 * 1024  # grows to two frames when a frame is larger
    # Feedback (NACKs, REGISTER) has its own budget, in bytes and in packets.
    # REGISTER is always sent but counts against it, so NACKs give way to it.
    feedback_kbps: int = 128
    feedback_pps: int = 200
    video_fec_k: int = 4
    audio_fec_k: int = 2
    audio_delay_frames: int = 3
    video_deadline: float = 0.3
    register_interval: float = 0.2
    register_timeout: float = 5.0
    keepalive_interval: float = 5.0
    switch_grace: float = 2.0
    socket_buffer: int = 1 << 20
    nack: bool = True  # ask the server to resend lost video packets (relay mode)
    nack_interval: float = 0.01


@dataclass
class SessionStats:
    packets_in: dict[str, int] = field(default_factory=lambda: dict.fromkeys(_KINDS, 0))
    bytes_in: dict[str, int] = field(default_factory=lambda: dict.fromkeys(_KINDS, 0))
    packets_out: int = 0
    bytes_out: int = 0
    video_frames_sent: int = 0
    video_frames_rate_limited: int = 0
    audio_frames_sent: int = 0
    audio_frames_rate_limited: int = 0
    rejected_source: int = 0
    unknown_ssrc: int = 0
    malformed: int = 0
    fec_recovered: int = 0
    nacks_sent: int = 0
    nacks_limited: int = 0  # NACKs not sent because the feedback budget was spent
    challenges: int = 0  # REGISTER_ACK challenges answered
    mixed_csrcs: set[int] = field(default_factory=set)
    frames_by_participant: dict[int, int] = field(default_factory=dict)
    packets_by_source: dict[str, int] = field(default_factory=lambda: {"server": 0, "peer": 0})


@dataclass
class _VideoIn:
    participant: int
    fec: FecDecoder = field(default_factory=FecDecoder)
    assembler: FrameAssembler = field(default_factory=FrameAssembler)
    nack: NackTracker | None = None
    parity_seen: bool = False  # the sender protects its frames with parity packets


VideoCallback = Callable[[int, VideoFrame], None]


def resolve_host(host: str) -> str:
    """Resolve a host name to the IP literal used for address comparisons."""
    try:
        return str(ipaddress.ip_address(host))
    except ValueError:
        return str(socket.getaddrinfo(host, None, type=socket.SOCK_DGRAM)[0][4][0])


class MediaSession(asyncio.DatagramProtocol):
    def __init__(
        self,
        server_host: str,
        joined: dict[str, Any],
        playout: AudioPlayout,
        on_video: VideoCallback,
        *,
        config: MediaConfig | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.config = config or MediaConfig()
        self.joined = joined
        self.playout = playout
        self.on_video = on_video
        self.clock = clock
        self.stats = SessionStats()
        self.server: Addr = (resolve_host(server_host), int(joined["media_port"]))
        self.participant_id = int(joined["participant_id"])
        self.mixer_ssrc = int(joined["mixer_ssrc"])
        self.mode = "relay"
        self.observed: Addr | None = None
        self._token = bytes.fromhex(joined["token"])
        self._video_tx = VideoPacketizer(int(joined["video_ssrc"]), self.config.video_fec_k)
        self._audio_tx = AudioPacketizer(int(joined["audio_ssrc"]), self.config.audio_fec_k)
        kbps = self.config.max_send_kbps
        rate = math.inf if kbps is None else kbps * 1000 / 8
        self._bucket = TokenBucket(rate, self.config.send_burst_bytes, clock)
        fb_rate = self.config.feedback_kbps * 1000 / 8
        self._fb_bytes = TokenBucket(fb_rate, fb_rate / 10, clock)
        fb_pps = self.config.feedback_pps
        self._fb_packets = TokenBucket(fb_pps, max(1.0, fb_pps / 10), clock)
        self._transport: asyncio.DatagramTransport | None = None
        self._targets: list[Addr] = [self.server]
        self._peer: Addr | None = None
        self._prev_peer: Addr | None = None
        self._grace_until = 0.0
        self._others = 0
        self._video_in: dict[int, _VideoIn] = {}
        self._audio_ssrcs: set[int] = {self.mixer_ssrc}
        self._audio_fec: dict[int, FecDecoder] = {}
        self._reg_txid = 0
        self._reg_future: asyncio.Future[Addr] | None = None
        self._tasks: list[asyncio.Task[None]] = []
        self._t0 = clock()
        self._video_ts_base = secrets.randbelow(1 << 32)
        self._audio_ts_base = secrets.randbelow(1 << 32) // AUDIO_SAMPLES_PER_FRAME * AUDIO_SAMPLES_PER_FRAME
        self._audio_n = 0  # frames of audio captured, plus the pauses
        self._last_audio_capture: float | None = None
        self._audio_marker = True  # the next audio frame sent starts a talk spurt

    # Life cycle -----------------------------------------------------------------

    async def start(self) -> Addr:
        """Open the UDP socket and register it with the server."""
        loop = asyncio.get_running_loop()
        server_ip = ipaddress.ip_address(self.server[0])
        if server_ip.version == 6:
            local = "::1" if server_ip.is_loopback else "::"
        else:
            local = "127.0.0.1" if server_ip.is_loopback else "0.0.0.0"
        transport, _ = await loop.create_datagram_endpoint(lambda: self, local_addr=(local, 0))
        sock = transport.get_extra_info("socket")
        if sock is not None:
            with contextlib.suppress(OSError):
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, self.config.socket_buffer)
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, self.config.socket_buffer)
        observed = await self.register()
        self._tasks.append(asyncio.create_task(self._keepalive(), name="media-keepalive"))
        self._tasks.append(asyncio.create_task(self._expire_loop(), name="media-expire"))
        if self.config.nack:
            self._tasks.append(asyncio.create_task(self._nack_loop(), name="media-nack"))
        return observed

    def connection_made(self, transport: asyncio.BaseTransport) -> None:
        assert isinstance(transport, asyncio.DatagramTransport)
        self._transport = transport

    @property
    def local_addr(self) -> Addr:
        assert self._transport is not None
        addr = self._transport.get_extra_info("sockname")
        return str(addr[0]), int(addr[1])

    async def register(self) -> Addr:
        """Send REGISTER until the server acknowledges it (like a STUN binding)."""
        loop = asyncio.get_running_loop()
        self._reg_txid = secrets.randbits(64)
        self._reg_future = loop.create_future()
        pkt = build_register(self._reg_txid, self._token)
        deadline = self.clock() + self.config.register_timeout
        while True:
            self._send_feedback(pkt, must=True)
            try:
                self.observed = await asyncio.wait_for(
                    asyncio.shield(self._reg_future), self.config.register_interval
                )
                return self.observed
            except TimeoutError:
                if self.clock() > deadline:
                    raise TimeoutError("media registration was not acknowledged") from None

    async def close(self) -> None:
        tasks, self._tasks = self._tasks, []
        try:
            await cancel_and_wait(tasks)
        finally:
            if self._transport is not None:
                self._transport.close()
                self._transport = None

    async def _keepalive(self) -> None:
        while True:
            await asyncio.sleep(self.config.keepalive_interval)
            self._send_feedback(build_register(secrets.randbits(64), self._token), must=True)

    async def _expire_loop(self) -> None:
        while True:
            await asyncio.sleep(0.1)
            now = self.clock()
            for v in self._video_in.values():
                v.assembler.expire(now)

    async def _nack_loop(self) -> None:
        own = int(self.joined["video_ssrc"])
        while True:
            await asyncio.sleep(self.config.nack_interval)
            now = self.clock()
            for ssrc, v in self._video_in.items():
                if v.nack is None:
                    continue
                if self._peer is not None:
                    # Only the server keeps a retransmission buffer, so P2P asks for
                    # nothing, and losses seen meanwhile are not counted as requests.
                    v.nack.reset()
                    continue
                for pkt in build_nacks(own, ssrc, v.nack.due(now)):
                    if self._send_feedback(pkt):
                        self.stats.nacks_sent += 1
                    else:
                        self.stats.nacks_limited += 1

    def nack_stats(self) -> NackStats:
        """NACK counters summed over the current senders."""
        total = NackStats()
        for v in self._video_in.values():
            if v.nack is not None:
                total.requested += v.nack.stats.requested
                total.recovered += v.nack.stats.recovered
                total.abandoned += v.nack.stats.abandoned
        return total

    # Control-plane updates ------------------------------------------------------

    def update_roster(self, roster: dict[str, Any]) -> None:
        video: dict[int, int] = {}
        audio: set[int] = {self.mixer_ssrc}
        others = 0
        for p in roster.get("participants", []):
            pid = int(p["participant_id"])
            if pid == self.participant_id:
                continue
            others += 1
            video[int(p["video_ssrc"])] = pid
            audio.add(int(p["audio_ssrc"]))
        for ssrc in list(self._video_in):
            if ssrc not in video:
                del self._video_in[ssrc]
        for ssrc, pid in video.items():
            self._video_in.setdefault(
                ssrc,
                _VideoIn(
                    pid,
                    assembler=FrameAssembler(self.config.video_deadline),
                    nack=NackTracker() if self.config.nack else None,
                ),
            )
        for ssrc in list(self._audio_fec):
            if ssrc not in audio:
                del self._audio_fec[ssrc]
        self._audio_ssrcs = audio
        self._others = others

    def update_mode(self, msg: dict[str, Any]) -> None:
        mode = str(msg.get("mode"))
        if mode == "p2p" and msg.get("peer"):
            peer = parse_addr(str(msg["peer"]["addr"]))
            if self._peer is not None and self._peer != peer:
                self._prev_peer = self._peer
            self._peer = peer
            self._targets = [peer]
        else:
            if self._peer is not None:
                self._prev_peer = self._peer
            self._peer = None
            self._targets = [self.server]
            mode = "relay"
        self.mode = mode
        self._grace_until = self.clock() + self.config.switch_grace

    @property
    def targets(self) -> list[Addr]:
        return list(self._targets)

    # Sending ----------------------------------------------------------------------

    def _send_raw(self, pkt: bytes, addr: Addr) -> None:
        if self._transport is None:
            return
        self._transport.sendto(pkt, addr)
        self.stats.packets_out += 1
        self.stats.bytes_out += len(pkt)

    def _send_feedback(self, pkt: bytes, *, must: bool = False) -> bool:
        """Send a NACK or REGISTER to the server within the feedback budget. With must,
        it is sent even when the budget is spent, and still counted."""
        cost = len(pkt) + _WIRE_OVERHEAD
        if must:
            self._fb_bytes.consume(cost)
            self._fb_packets.consume(1)
        elif self._fb_bytes.available() < cost or self._fb_packets.available() < 1:
            return False
        else:
            self._fb_bytes.consume(cost)
            self._fb_packets.consume(1)
        self._send_raw(pkt, self.server)
        return True

    def _send_packets(self, pkts: list[bytes]) -> None:
        for addr in self._targets:
            for pkt in pkts:
                self._send_raw(pkt, addr)

    def _cost(self, packets: int, size: int) -> int:
        return (size + packets * _WIRE_OVERHEAD) * len(self._targets)

    def send_video(self, jpeg: bytes, capture_time: float) -> int | None:
        """Send one JPEG frame; returns its frame id, or None if it was not sent
        (nobody to send to, or the bitrate cap has no room for the whole frame). The
        budget is checked before the frame is packetized, so a frame that is not sent
        uses no sequence numbers and receivers see no loss. Raises ProtocolError for a
        frame the wire format cannot carry (over MAX_FRAME_BYTES)."""
        if self._others == 0 or self._transport is None:
            return None
        cost = self._cost(*self._video_tx.wire_size(len(jpeg)))
        if cost > self._bucket.burst:
            # A frame larger than the burst could never pass: make room for two such
            # frames. The rate cap is unchanged; large frames just go out less often.
            self._bucket.grow(2 * cost)
        if not self._bucket.try_consume(cost):
            self.stats.video_frames_rate_limited += 1
            return None
        ts = (self._video_ts_base + round((capture_time - self._t0) * VIDEO_CLOCK_RATE)) & 0xFFFFFFFF
        frame_id, pkts = self._video_tx.packetize(jpeg, ts)
        self._send_packets(pkts)
        self.stats.video_frames_sent += 1
        return frame_id

    def send_audio(self, pcm_be: bytes, capture_time: float) -> bool:
        """Send one 20 ms frame (big-endian L16). The RTP timestamp advances 320 samples
        per captured frame, whether or not the frame is sent. The capture clock only
        detects a pause: a frame that comes more than _AUDIO_GAP_FRAMES frames after the
        previous one moves the timestamp over the pause and sets the marker, while a late
        callback followed by a quick one does not shift the stream."""
        if self._last_audio_capture is not None:
            elapsed = round((capture_time - self._last_audio_capture) / AUDIO_FRAME_SECONDS)
            if elapsed > _AUDIO_GAP_FRAMES:
                self._audio_n += elapsed
                self._audio_marker = True
            else:
                self._audio_n += 1
        self._last_audio_capture = capture_time
        # A frame that is not sent still advances the timestamp, so the stream keeps
        # time with the capture clock.
        if self._others == 0 or self._transport is None:
            self._audio_marker = True  # the next frame sent starts a talk spurt
            return False
        if not self._bucket.try_consume(self._cost(*self._audio_tx.wire_size())):
            self.stats.audio_frames_rate_limited += 1  # a loss to the receivers
            return False
        ts = (self._audio_ts_base + self._audio_n * AUDIO_SAMPLES_PER_FRAME) & 0xFFFFFFFF
        pkts = self._audio_tx.packetize(pcm_be, ts, self._audio_marker)
        self._audio_marker = False
        self._send_packets(pkts)
        self.stats.audio_frames_sent += 1
        return True

    # Receiving --------------------------------------------------------------------

    def _source(self, addr: Addr) -> str | None:
        if addr == self.server:
            return "server"
        if addr == self._peer:
            return "peer"
        if addr == self._prev_peer and self.clock() < self._grace_until:
            return "peer"
        return None

    def datagram_received(self, data: bytes, addr: tuple[Any, ...]) -> None:
        src: Addr = (str(addr[0]), int(addr[1]))
        if not data:
            return
        if data[0] == TYPE_REGISTER_ACK:
            self._on_ack(data, src)
            return
        if not is_rtp(data):
            self.stats.malformed += 1
            return
        source = self._source(src)
        if source is None:
            self.stats.rejected_source += 1
            return
        ssrc, pt, seq = peek_ssrc(data)
        now = self.clock()
        try:
            if ssrc in self._video_in:
                self._on_video(self._video_in[ssrc], pt, seq, data, now)
            elif ssrc in self._audio_ssrcs:
                self._on_audio(ssrc, pt, seq, data)
            else:
                self.stats.unknown_ssrc += 1
                return
        except ProtocolError:
            self.stats.malformed += 1
            return
        self.stats.packets_by_source[source] += 1

    def _count(self, kind: str, n: int) -> None:
        self.stats.packets_in[kind] += 1
        self.stats.bytes_in[kind] += n

    def _on_ack(self, data: bytes, src: Addr) -> None:
        if src != self.server:
            return
        try:
            ack = parse_register_ack(data)
        except ProtocolError:
            return
        if ack.challenge:
            # The server wants proof that this socket receives what it sends here
            # (first registration, a new NAT mapping, or an expired binding): echo the
            # nonce from the same socket. Only the echo moves the binding.
            self.stats.challenges += 1
            self._send_feedback(build_register(ack.txid, self._token, ack.nonce), must=True)
            return
        fut = self._reg_future
        if fut is not None and not fut.done() and ack.txid == self._reg_txid:
            fut.set_result(ack.observed)

    def _on_video(self, v: _VideoIn, pt: int, seq: int, data: bytes, now: float) -> None:
        if pt == PT_FEC:
            self._count("video_fec", len(data))
            v.parity_seen = True
            repaired = v.fec.on_parity(data)
            packets = []
        elif pt == PT_VIDEO:
            self._count("video", len(data))
            repaired = v.fec.on_media(seq, data)
            packets = [data]
        else:
            return
        self.stats.fec_recovered += len(repaired)
        if v.nack is not None:
            v.nack.on_packet(seq, now)
            for pkt in repaired:
                v.nack.on_packet(peek_ssrc(pkt)[2], now)
        for pkt in packets + repaired:
            header, payload = parse_rtp(pkt)
            frame = v.assembler.push(header, payload, now)
            if frame is not None:
                if v.nack is not None and frame.first_seq >= 0:
                    # Every fragment is here, so whatever is still missing between the
                    # first fragment and the parity packet after the last is parity:
                    # not worth a retransmission.
                    span = seq_diff(frame.last_seq, frame.first_seq) + 1 + v.parity_seen
                    v.nack.forget(frame.first_seq, span)
                counts = self.stats.frames_by_participant
                counts[v.participant] = counts.get(v.participant, 0) + 1
                self.on_video(v.participant, frame)

    def _on_audio(self, ssrc: int, pt: int, seq: int, data: bytes) -> None:
        fec = self._audio_fec.setdefault(ssrc, FecDecoder(history=64))
        if pt == PT_FEC:
            self._count("audio_fec", len(data))
            packets = fec.on_parity(data)
            self.stats.fec_recovered += len(packets)
        elif pt == PT_AUDIO:
            self._count("audio", len(data))
            repaired = fec.on_media(seq, data)
            self.stats.fec_recovered += len(repaired)
            packets = [data, *repaired]
        else:
            return
        for pkt in packets:
            header, payload = parse_rtp(pkt)
            if ssrc == self.mixer_ssrc:
                self.stats.mixed_csrcs.update(header.csrc)
            self.playout.push(ssrc, header.timestamp, payload)

    def error_received(self, exc: Exception) -> None:
        # ICMP errors (for example port unreachable while a peer restarts) are expected
        # on UDP; the session keeps running.
        return
