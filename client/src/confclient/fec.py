"""XOR forward error correction (docs/PROTOCOL.md section 2.6).

One parity packet protects k consecutive packets of a stream and can rebuild any
single missing one. XOR over packet bodies is done on Python integers, which runs in
C and is fast enough for a few thousand packets per second.
"""

from __future__ import annotations

import struct
from collections import OrderedDict
from collections.abc import Sequence

from .protocol import (
    MAX_FEC_GROUP,
    MAX_PROTECTED_BODY,
    PT_FEC,
    RTP_HEADER_SIZE,
    RTP_VERSION,
    Buffer,
    FecHeader,
    ProtocolError,
    RtpHeader,
    parse_fec,
    parse_rtp,
)

_SEQ = struct.Struct("!H")
_TS = struct.Struct("!I")


def _body_int(body: Buffer) -> int:
    """The body, zero-padded to MAX_PROTECTED_BODY, as one big-endian integer."""
    return int.from_bytes(bytes(body).ljust(MAX_PROTECTED_BODY, b"\0"), "big")


class FecEncoder:
    """Accumulates parity over consecutive packets of one RTP stream."""

    def __init__(self) -> None:
        self._reset()

    def _reset(self) -> None:
        self._count = 0
        self._base = 0
        self._next = 0
        self._b0 = 0
        self._b1 = 0
        self._length = 0
        self._ts = 0
        self._last_ts = 0
        self._max_body = 0
        self._parity = 0

    def __len__(self) -> int:
        return self._count

    def add(self, pkt: bytes) -> None:
        """Fold a complete RTP packet into the current group."""
        if len(pkt) < RTP_HEADER_SIZE:
            raise ProtocolError("packet too short for FEC")
        body = memoryview(pkt)[RTP_HEADER_SIZE:]
        if len(body) > MAX_PROTECTED_BODY:
            raise ProtocolError("packet too large for FEC")
        (seq,) = _SEQ.unpack_from(pkt, 2)
        if self._count == 0:
            self._base = seq
        elif seq != self._next:
            raise ProtocolError("FEC group packets must be consecutive")
        if self._count >= MAX_FEC_GROUP:
            raise ProtocolError("FEC group too large")
        (ts,) = _TS.unpack_from(pkt, 4)
        self._count += 1
        self._next = (seq + 1) & 0xFFFF
        self._b0 ^= pkt[0] & 0x3F
        self._b1 ^= pkt[1]
        self._length ^= len(body)
        self._ts ^= ts
        self._last_ts = ts
        self._max_body = max(self._max_body, len(body))
        self._parity ^= _body_int(body)

    def flush(self, ssrc: int, seq: int) -> bytes:
        """Return the parity packet of the current group and start a new group."""
        if self._count == 0:
            raise ProtocolError("empty FEC group")
        header = RtpHeader(payload_type=PT_FEC, sequence=seq, timestamp=self._last_ts, ssrc=ssrc).encode()
        fec = FecHeader(self._base, self._count, self._b0, self._b1, self._length, self._ts).encode()
        parity = self._parity.to_bytes(MAX_PROTECTED_BODY, "big")[: self._max_body]
        self._reset()
        return header + fec + parity


def recover(parity_pkt: Buffer, present: Sequence[Buffer], missing_seq: int) -> bytes:
    """Rebuild the packet with sequence number missing_seq from its group's parity
    packet and the k-1 packets that arrived."""
    hdr, payload = parse_rtp(parity_pkt)
    if hdr.payload_type != PT_FEC:
        raise ProtocolError("not a parity packet")
    fec, parity = parse_fec(payload)
    if len(present) != fec.count - 1 or not fec.covers(missing_seq):
        raise ProtocolError("group does not have exactly one missing packet")
    b0, b1, length, ts = fec.byte0_rec, fec.byte1_rec, fec.length_rec, fec.ts_rec
    acc = _body_int(parity)
    for pkt in present:
        body = memoryview(pkt)[RTP_HEADER_SIZE:]
        if len(body) > len(parity):
            raise ProtocolError("protected packet longer than parity")
        b0 ^= pkt[0] & 0x3F
        b1 ^= pkt[1]
        length ^= len(body)
        (pts,) = _TS.unpack_from(pkt, 4)
        ts ^= pts
        acc ^= _body_int(body)
    if length > len(parity):
        raise ProtocolError("recovered length exceeds parity")
    rebuilt = acc.to_bytes(MAX_PROTECTED_BODY, "big")[:length]
    head = struct.pack("!BBHII", (RTP_VERSION << 6) | (b0 & 0x3F), b1, missing_seq, ts, hdr.ssrc)
    return head + rebuilt


class FecDecoder:
    """Recovery bookkeeping for one stream: remembers recent packets and parity packets,
    and rebuilds a packet whenever a group with parity is missing exactly one member.
    """

    def __init__(self, history: int = 256) -> None:
        self._history = history
        self._packets: OrderedDict[int, bytes] = OrderedDict()
        self._parities: OrderedDict[int, tuple[FecHeader, bytes]] = OrderedDict()
        self.recovered = 0

    def _remember(self, seq: int, pkt: bytes) -> None:
        self._packets[seq] = pkt
        self._packets.move_to_end(seq)
        while len(self._packets) > self._history:
            self._packets.popitem(last=False)

    def on_media(self, seq: int, pkt: bytes) -> list[bytes]:
        """Record a received media packet; returns packets this completes a recovery for."""
        if seq in self._packets:
            return []
        self._remember(seq, pkt)
        out: list[bytes] = []
        for key, (fec, parity) in list(self._parities.items()):
            if fec.covers(seq):
                out.extend(self._try(key, fec, parity))
        return out

    def on_parity(self, pkt: bytes) -> list[bytes]:
        """Record a parity packet; returns the packet it rebuilds, if any."""
        _hdr, payload = parse_rtp(pkt)
        fec, _ = parse_fec(payload)
        if fec.sn_base in self._parities:
            return []
        self._parities[fec.sn_base] = (fec, pkt)
        while len(self._parities) > self._history // 4:
            self._parities.popitem(last=False)
        return self._try(fec.sn_base, fec, pkt)

    def _try(self, key: int, fec: FecHeader, parity: bytes) -> list[bytes]:
        seqs = fec.sequences()
        missing = [s for s in seqs if s not in self._packets]
        if len(missing) != 1:
            if not missing:
                del self._parities[key]
            return []
        del self._parities[key]
        try:
            pkt = recover(parity, [self._packets[s] for s in seqs if s != missing[0]], missing[0])
        except ProtocolError:
            return []
        self._remember(missing[0], pkt)
        self.recovered += 1
        return [pkt]
