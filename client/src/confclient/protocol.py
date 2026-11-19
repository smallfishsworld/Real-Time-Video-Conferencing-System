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
from collections.abc import Iterable
from dataclasses import dataclass
from typing import Any, NamedTuple

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
REGISTER_VERSION = 2  # version 2 added the return-routability nonce
REGISTER_SIZE = 36
TOKEN_SIZE = 16
NONCE_SIZE = 8
REGISTER_FLAG_NONCE = 0x01  # the REGISTER echoes a challenge's nonce
ACK_FLAG_CHALLENGE = 0x01  # the ACK is a challenge: the binding has not moved yet

# RTCP generic NACK (RFC 4585): a receiver asks the server to resend video packets.
RTCP_RTPFB = 205
NACK_FORMAT = 1
NACK_HEADER_SIZE = 12
MAX_NACK_ITEMS = 16  # (PID, BLP) pairs per NACK: up to 272 sequence numbers

MAX_CONTROL_MESSAGE = 64 * 1024
CONTROL_VERSION = 1

_RTP = struct.Struct("!BBHII")
_VIDEO = struct.Struct("!IHH")
_FEC = struct.Struct("!HBBBBHI")
_REGISTER = struct.Struct("!BBBBQ")
_ACK = struct.Struct("!BBBBQH")
_LEN = struct.Struct("!I")
_NACK = struct.Struct("!BBHII")
_NACK_ITEM = struct.Struct("!HH")

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


def nack_items(lost: Iterable[int]) -> list[tuple[int, int]]:
    """Pack lost sequence numbers, in increasing order modulo 2**16, into (PID, BLP)
    pairs: bit i of BLP set means PID + i + 1 is lost too."""
    items: list[list[int]] = []
    for seq in lost:
        if items:
            d = (seq - items[-1][0]) & 0xFFFF
            if 1 <= d <= 16:
                items[-1][1] |= 1 << (d - 1)
                continue
        items.append([seq & 0xFFFF, 0])
    return [(pid, blp) for pid, blp in items]


def _nack(sender_ssrc: int, media_ssrc: int, items: list[tuple[int, int]]) -> bytes:
    head = _NACK.pack(0x80 | NACK_FORMAT, RTCP_RTPFB, 2 + len(items), sender_ssrc, media_ssrc)
    return head + b"".join(_NACK_ITEM.pack(pid, blp) for pid, blp in items)


def build_nack(sender_ssrc: int, media_ssrc: int, lost: Iterable[int]) -> bytes:
    """One generic NACK asking for `lost` packets of `media_ssrc`."""
    items = nack_items(lost)
    if not 1 <= len(items) <= MAX_NACK_ITEMS:
        raise ProtocolError(f"a NACK carries 1 to {MAX_NACK_ITEMS} items, not {len(items)}")
    return _nack(sender_ssrc, media_ssrc, items)


def build_nacks(sender_ssrc: int, media_ssrc: int, lost: Iterable[int]) -> list[bytes]:
    """As many NACKs as it takes to ask for every sequence number in `lost`."""
    items = nack_items(lost)
    return [
        _nack(sender_ssrc, media_ssrc, items[i : i + MAX_NACK_ITEMS])
        for i in range(0, len(items), MAX_NACK_ITEMS)
    ]


def is_nack(pkt: Buffer) -> bool:
    b = memoryview(pkt)
    return len(b) >= 2 and b[0] >> 6 == RTP_VERSION and b[0] & 0x1F == NACK_FORMAT and b[1] == RTCP_RTPFB


def parse_nack(pkt: Buffer) -> tuple[int, int, list[int]]:
    """Decode a generic NACK into (sender SSRC, media SSRC, lost sequence numbers)."""
    b = memoryview(pkt)
    if len(b) < NACK_HEADER_SIZE + 4:
        raise ProtocolError("NACK too short")
    if not is_nack(b):
        raise ProtocolError("not a generic NACK")
    b0, _pt, length, sender, media = _NACK.unpack_from(b, 0)
    if b0 & 0x20:
        raise ProtocolError("NACK with padding")
    size = (length + 1) * 4
    if size > len(b):
        raise ProtocolError("NACK length beyond the packet")
    count = (size - NACK_HEADER_SIZE) // 4
    if size != len(b) or not 1 <= count <= MAX_NACK_ITEMS:
        raise ProtocolError("invalid NACK length")
    lost: list[int] = []
    for i in range(count):
        pid, blp = _NACK_ITEM.unpack_from(b, NACK_HEADER_SIZE + 4 * i)
        lost.append(pid)
        lost.extend((pid + bit + 1) & 0xFFFF for bit in range(16) if blp >> bit & 1)
    return sender, media, lost


def build_register(txid: int, token: bytes, nonce: bytes | None = None) -> bytes:
    """A REGISTER; nonce is the one from the last challenge, echoed to prove that this
    address receives what the server sends to it."""
    if len(token) != TOKEN_SIZE:
        raise ProtocolError("token must be 16 bytes")
    if nonce is not None and len(nonce) != NONCE_SIZE:
        raise ProtocolError("nonce must be 8 bytes")
    flags = 0 if nonce is None else REGISTER_FLAG_NONCE
    return (
        _REGISTER.pack(TYPE_REGISTER, REGISTER_VERSION, flags, 0, txid) + token + (nonce or bytes(NONCE_SIZE))
    )


def parse_register(b: Buffer) -> tuple[int, bytes, bytes | None]:
    """Return (transaction id, token, echoed nonce or None)."""
    if len(b) < REGISTER_SIZE:
        raise ProtocolError("REGISTER too short")
    typ, version, flags, _reserved, txid = _REGISTER.unpack_from(b, 0)
    if typ != TYPE_REGISTER or version != REGISTER_VERSION or flags & ~REGISTER_FLAG_NONCE:
        raise ProtocolError("not a REGISTER packet")
    nonce = bytes(b[28:36]) if flags & REGISTER_FLAG_NONCE else None
    return txid, bytes(b[12:28]), nonce


class RegisterAck(NamedTuple):
    txid: int
    observed: tuple[str, int]
    challenge: bool  # the binding has not moved: send REGISTER again echoing nonce
    nonce: bytes


def build_register_ack(
    txid: int, host: str, port: int, *, challenge: bool = False, nonce: bytes = bytes(NONCE_SIZE)
) -> bytes:
    addr = ipaddress.ip_address(host)
    family = 4 if addr.version == 4 else 6
    flags = ACK_FLAG_CHALLENGE if challenge else 0
    return _ACK.pack(TYPE_REGISTER_ACK, REGISTER_VERSION, family, flags, txid, port) + addr.packed + nonce


def parse_register_ack(b: Buffer) -> RegisterAck:
    if len(b) < 14:
        raise ProtocolError("REGISTER_ACK too short")
    typ, version, family, flags, txid, port = _ACK.unpack_from(b, 0)
    if typ != TYPE_REGISTER_ACK or version != REGISTER_VERSION or flags & ~ACK_FLAG_CHALLENGE:
        raise ProtocolError("not a REGISTER_ACK packet")
    alen = {4: 4, 6: 16}.get(family)
    if alen is None:
        raise ProtocolError("bad address family")
    if len(b) < 14 + alen + NONCE_SIZE:
        raise ProtocolError("REGISTER_ACK too short")
    raw = bytes(b[14 : 14 + alen])
    host = str(ipaddress.IPv4Address(raw) if alen == 4 else ipaddress.IPv6Address(raw))
    nonce = bytes(b[14 + alen : 14 + alen + NONCE_SIZE])
    return RegisterAck(txid, (host, port), bool(flags & ACK_FLAG_CHALLENGE), nonce)


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
    except (ValueError, RecursionError) as exc:
        # ValueError covers bad UTF-8, bad JSON and integers too long to convert;
        # RecursionError, nesting deeper than the interpreter's stack.
        raise ProtocolError("malformed control message") from exc
    if not isinstance(msg, dict) or not isinstance(msg.get("type"), str) or not msg["type"]:
        raise ProtocolError("control message without type")
    return msg


async def read_frame(reader: asyncio.StreamReader) -> dict[str, Any]:
    """Read exactly one control message from an asyncio stream."""
    header = await reader.readexactly(4)
    (n,) = _LEN.unpack(header)
    if n == 0 or n > MAX_CONTROL_MESSAGE:
        raise ProtocolError(f"invalid control frame length {n}")
    return decode_body(await reader.readexactly(n))
