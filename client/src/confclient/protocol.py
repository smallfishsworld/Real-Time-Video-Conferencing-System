"""Wire formats of docs/PROTOCOL.md: RTP header, video fragment header, FEC header,
UDP registration packets and length-prefixed JSON control messages.

This module mirrors server/internal/protocol and is tested against the same golden
vectors (protocol-testdata/vectors.json).
"""

from __future__ import annotations

import asyncio
import ipaddress
import json
import struct
from dataclasses import dataclass
from typing import Any

RTP_VERSION = 2
RTP_HEADER_SIZE = 12
MAX_CSRC = 15

PT_VIDEO = 96
PT_AUDIO = 97
PT_FEC = 127

VIDEO_CLOCK_RATE = 90_000
AUDIO_CLOCK_RATE = 16_000
AUDIO_SAMPLES_PER_FRAME = 320  # 20 ms at 16 kHz
AUDIO_FRAME_BYTES = AUDIO_SAMPLES_PER_FRAME * 2
AUDIO_FRAME_SECONDS = AUDIO_SAMPLES_PER_FRAME / AUDIO_CLOCK_RATE

MAX_DATAGRAM = 1500
VIDEO_HEADER_SIZE = 8
MAX_FRAGMENT_PAYLOAD = 1200
MAX_FRAGMENTS = 1024
MAX_FRAME_BYTES = MAX_FRAGMENTS * MAX_FRAGMENT_PAYLOAD

FEC_HEADER_SIZE = 12
MAX_FEC_GROUP = 48
MAX_SEND_DATAGRAM = RTP_HEADER_SIZE + FEC_HEADER_SIZE + VIDEO_HEADER_SIZE + MAX_FRAGMENT_PAYLOAD
MAX_PROTECTED_BODY = MAX_SEND_DATAGRAM - RTP_HEADER_SIZE - FEC_HEADER_SIZE

TYPE_REGISTER = 0x01
TYPE_REGISTER_ACK = 0x02
REGISTER_VERSION = 1
REGISTER_SIZE = 28
TOKEN_SIZE = 16

MAX_CONTROL_MESSAGE = 64 * 1024
CONTROL_VERSION = 1

_RTP = struct.Struct("!BBHII")
_VIDEO = struct.Struct("!IHH")
_FEC = struct.Struct("!HBBBBHI")
_REGISTER = struct.Struct("!BBHQ")
_ACK = struct.Struct("!BBBBQH")
_LEN = struct.Struct("!I")

Buffer = bytes | bytearray | memoryview


class ProtocolError(ValueError):
    """Raised for packets or messages that violate the protocol."""


@dataclass(frozen=True, slots=True)
class RtpHeader:
    payload_type: int
    sequence: int
    timestamp: int
    ssrc: int
    marker: bool = False
    csrc: tuple[int, ...] = ()

    def encode(self) -> bytes:
        if not 0 <= self.payload_type <= 0x7F or len(self.csrc) > MAX_CSRC:
            raise ProtocolError("invalid RTP header fields")
        b0 = (RTP_VERSION << 6) | len(self.csrc)
        b1 = (0x80 if self.marker else 0) | self.payload_type
        out = _RTP.pack(b0, b1, self.sequence & 0xFFFF, self.timestamp & 0xFFFFFFFF, self.ssrc & 0xFFFFFFFF)
        if self.csrc:
            out += struct.pack(f"!{len(self.csrc)}I", *self.csrc)
        return out


def is_rtp(pkt: Buffer) -> bool:
    return len(pkt) >= RTP_HEADER_SIZE and pkt[0] >> 6 == RTP_VERSION


def parse_rtp(pkt: Buffer) -> tuple[RtpHeader, memoryview]:
    """Decode an RTP packet; returns the header and the payload (padding stripped,
    header extension skipped)."""
    if len(pkt) < RTP_HEADER_SIZE:
        raise ProtocolError("RTP packet too short")
    b0, b1, seq, ts, ssrc = _RTP.unpack_from(pkt, 0)
    if b0 >> 6 != RTP_VERSION:
        raise ProtocolError("unsupported RTP version")
    cc = b0 & 0x0F
    off = RTP_HEADER_SIZE + 4 * cc
    if len(pkt) < off:
        raise ProtocolError("CSRC list truncated")
    csrc = struct.unpack_from(f"!{cc}I", pkt, RTP_HEADER_SIZE) if cc else ()
    if b0 & 0x10:
        if len(pkt) < off + 4:
            raise ProtocolError("header extension truncated")
        (words,) = struct.unpack_from("!H", pkt, off + 2)
        off += 4 + 4 * words
        if len(pkt) < off:
            raise ProtocolError("header extension truncated")
    end = len(pkt)
    if b0 & 0x20:
        pad = pkt[end - 1]
        if pad == 0 or end - pad < off:
            raise ProtocolError("invalid RTP padding")
        end -= pad
    header = RtpHeader(
        payload_type=b1 & 0x7F, sequence=seq, timestamp=ts, ssrc=ssrc, marker=bool(b1 & 0x80), csrc=csrc
    )
    return header, memoryview(pkt)[off:end]


def peek_ssrc(pkt: Buffer) -> tuple[int, int, int]:
    """Return (ssrc, payload type, sequence) of an RTP packet without a full parse."""
    _b0, b1, seq, _ts, ssrc = _RTP.unpack_from(pkt, 0)
    return ssrc, b1 & 0x7F, seq


def seq_diff(a: int, b: int) -> int:
    """Signed distance a - b in 16-bit sequence space."""
    d = (a - b) & 0xFFFF
    return d - 0x10000 if d >= 0x8000 else d


def u32_newer(a: int, b: int) -> bool:
    """True when a is after b in 32-bit wrap-around order."""
    d = (a - b) & 0xFFFFFFFF
    return 0 < d < 0x80000000


@dataclass(frozen=True, slots=True)
class VideoHeader:
    frame_id: int
    index: int
    count: int

    def encode(self) -> bytes:
        if not 0 < self.count <= MAX_FRAGMENTS or not 0 <= self.index < self.count:
            raise ProtocolError("invalid video header")
        return _VIDEO.pack(self.frame_id & 0xFFFFFFFF, self.index, self.count)


def parse_video(payload: Buffer) -> tuple[VideoHeader, memoryview]:
    if len(payload) < VIDEO_HEADER_SIZE + 1:
        raise ProtocolError("video payload too short")
    frame_id, index, count = _VIDEO.unpack_from(payload, 0)
    if count == 0 or count > MAX_FRAGMENTS or index >= count:
        raise ProtocolError("invalid fragment index/count")
    frag = memoryview(payload)[VIDEO_HEADER_SIZE:]
    if len(frag) > MAX_FRAGMENT_PAYLOAD:
        raise ProtocolError("fragment too large")
    return VideoHeader(frame_id, index, count), frag


def fragment_count(frame_len: int) -> int:
    return (frame_len + MAX_FRAGMENT_PAYLOAD - 1) // MAX_FRAGMENT_PAYLOAD


@dataclass(frozen=True, slots=True)
class FecHeader:
    sn_base: int
    count: int
    byte0_rec: int
    byte1_rec: int
    length_rec: int
    ts_rec: int

    def encode(self) -> bytes:
        if not 0 < self.count <= MAX_FEC_GROUP:
            raise ProtocolError("invalid FEC group size")
        return _FEC.pack(
            self.sn_base, self.count, 0, self.byte0_rec, self.byte1_rec, self.length_rec, self.ts_rec
        )

    def covers(self, seq: int) -> bool:
        return (seq - self.sn_base) & 0xFFFF < self.count

    def sequences(self) -> list[int]:
        return [(self.sn_base + i) & 0xFFFF for i in range(self.count)]


def parse_fec(payload: Buffer) -> tuple[FecHeader, memoryview]:
    if len(payload) < FEC_HEADER_SIZE:
        raise ProtocolError("FEC header too short")
    sn_base, count, _reserved, b0, b1, length, ts = _FEC.unpack_from(payload, 0)
    if count == 0 or count > MAX_FEC_GROUP or b0 & 0xC0:
        raise ProtocolError("invalid FEC header")
    parity = memoryview(payload)[FEC_HEADER_SIZE:]
    if len(parity) > MAX_PROTECTED_BODY:
        raise ProtocolError("FEC parity too long")
    return FecHeader(sn_base, count, b0, b1, length, ts), parity


def build_register(txid: int, token: bytes) -> bytes:
    if len(token) != TOKEN_SIZE:
        raise ProtocolError("token must be 16 bytes")
    return _REGISTER.pack(TYPE_REGISTER, REGISTER_VERSION, 0, txid) + token


def parse_register(b: Buffer) -> tuple[int, bytes]:
    if len(b) < REGISTER_SIZE:
        raise ProtocolError("REGISTER too short")
    typ, version, _reserved, txid = _REGISTER.unpack_from(b, 0)
    if typ != TYPE_REGISTER or version != REGISTER_VERSION:
        raise ProtocolError("not a REGISTER packet")
    return txid, bytes(b[12:28])


def build_register_ack(txid: int, host: str, port: int) -> bytes:
    addr = ipaddress.ip_address(host)
    family = 4 if addr.version == 4 else 6
    return _ACK.pack(TYPE_REGISTER_ACK, REGISTER_VERSION, family, 0, txid, port) + addr.packed


def parse_register_ack(b: Buffer) -> tuple[int, tuple[str, int]]:
    """Return (transaction id, observed (host, port))."""
    if len(b) < 14:
        raise ProtocolError("REGISTER_ACK too short")
    typ, version, family, _reserved, txid, port = _ACK.unpack_from(b, 0)
    if typ != TYPE_REGISTER_ACK or version != REGISTER_VERSION:
        raise ProtocolError("not a REGISTER_ACK packet")
    if family == 4 and len(b) >= 18:
        host = str(ipaddress.IPv4Address(bytes(b[14:18])))
    elif family == 6 and len(b) >= 30:
        host = str(ipaddress.IPv6Address(bytes(b[14:30])))
    else:
        raise ProtocolError("bad address family")
    return txid, (host, port)


def parse_addr(addr: str) -> tuple[str, int]:
    """Parse "ip:port" or "[ipv6]:port" as sent in `mode` messages."""
    host, sep, port = addr.rpartition(":")
    if not sep:
        raise ProtocolError(f"bad address {addr!r}")
    host = host.strip("[]")
    ipaddress.ip_address(host)
    return host, int(port)


# Control plane --------------------------------------------------------------------


def encode_frame(msg: dict[str, Any]) -> bytes:
    body = json.dumps(msg, separators=(",", ":"), ensure_ascii=False).encode()
    if len(body) > MAX_CONTROL_MESSAGE:
        raise ProtocolError("control message too large")
    return _LEN.pack(len(body)) + body


def decode_body(body: bytes) -> dict[str, Any]:
    try:
        msg = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProtocolError("malformed control message") from exc
    if not isinstance(msg, dict) or not isinstance(msg.get("type"), str) or not msg["type"]:
        raise ProtocolError("control message without type")
    return msg


class FrameDecoder:
    """Incremental decoder for the length-prefixed control stream. It makes no
    assumption about how the byte stream was split into reads."""

    def __init__(self) -> None:
        self._buf = bytearray()

    def feed(self, data: bytes) -> list[dict[str, Any]]:
        self._buf += data
        out: list[dict[str, Any]] = []
        while len(self._buf) >= 4:
            (n,) = _LEN.unpack_from(self._buf, 0)
            if n == 0 or n > MAX_CONTROL_MESSAGE:
                raise ProtocolError(f"invalid control frame length {n}")
            if len(self._buf) < 4 + n:
                break
            body = bytes(self._buf[4 : 4 + n])
            del self._buf[: 4 + n]
            out.append(decode_body(body))
        return out


async def read_frame(reader: asyncio.StreamReader) -> dict[str, Any]:
    """Read exactly one control message from an asyncio stream."""
    header = await reader.readexactly(4)
    (n,) = _LEN.unpack(header)
    if n == 0 or n > MAX_CONTROL_MESSAGE:
        raise ProtocolError(f"invalid control frame length {n}")
    return decode_body(await reader.readexactly(n))
