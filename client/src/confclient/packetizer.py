"""Turn JPEG frames and 20 ms audio frames into RTP packets (plus FEC parity)."""

from __future__ import annotations

import secrets

from .fec import FecEncoder
from .protocol import (
    AUDIO_FRAME_BYTES,
    MAX_FRAGMENT_PAYLOAD,
    MAX_FRAME_BYTES,
    PT_AUDIO,
    PT_VIDEO,
    ProtocolError,
    RtpHeader,
    VideoHeader,
    fragment_count,
)


class _Stream:
    def __init__(self, ssrc: int, fec_k: int) -> None:
        if fec_k < 0:
            raise ValueError("fec_k must be >= 0")
        self.ssrc = ssrc
        self.fec_k = fec_k
        self.seq = secrets.randbelow(1 << 16)
        self._fec = FecEncoder()

    def _next_seq(self) -> int:
        seq = self.seq
        self.seq = (self.seq + 1) & 0xFFFF
        return seq

    def _protect(self, pkt: bytes, out: list[bytes], close_group: bool) -> None:
        if self.fec_k == 0:
            return
        self._fec.add(pkt)
        if len(self._fec) >= self.fec_k or close_group:
            out.append(self._fec.flush(self.ssrc, self._next_seq()))


class VideoPacketizer(_Stream):
    """Splits JPEG frames into fragments of at most 1200 bytes. With fec_k > 0 a parity
    packet follows every fec_k fragments; groups never span two frames, so a frame can
    be repaired without waiting for the next one."""

    def __init__(self, ssrc: int, fec_k: int = 4) -> None:
        super().__init__(ssrc, fec_k)
        self.frame_id = secrets.randbelow(1 << 32)

    def packetize(self, jpeg: bytes, timestamp: int) -> tuple[int, list[bytes]]:
        """Return (frame id, packets) for one frame captured at RTP time timestamp."""
        if not jpeg or len(jpeg) > MAX_FRAME_BYTES:
            raise ProtocolError(f"frame of {len(jpeg)} bytes cannot be sent")
        frame_id = self.frame_id
        self.frame_id = (self.frame_id + 1) & 0xFFFFFFFF
        count = fragment_count(len(jpeg))
        out: list[bytes] = []
        for i in range(count):
            chunk = jpeg[i * MAX_FRAGMENT_PAYLOAD : (i + 1) * MAX_FRAGMENT_PAYLOAD]
            last = i == count - 1
            header = RtpHeader(
                payload_type=PT_VIDEO,
                sequence=self._next_seq(),
                timestamp=timestamp,
                ssrc=self.ssrc,
                marker=last,
            )
            pkt = header.encode() + VideoHeader(frame_id, i, count).encode() + chunk
            out.append(pkt)
            self._protect(pkt, out, close_group=last)
        return frame_id, out


class AudioPacketizer(_Stream):
    """One RTP packet per 20 ms frame; with fec_k > 0 a parity packet follows every
    fec_k frames."""

    def __init__(self, ssrc: int, fec_k: int = 2) -> None:
        super().__init__(ssrc, fec_k)

    def packetize(self, pcm_be: bytes, timestamp: int, marker: bool = False) -> list[bytes]:
        if len(pcm_be) != AUDIO_FRAME_BYTES:
            raise ProtocolError("audio frames must be 320 samples")
        header = RtpHeader(
            payload_type=PT_AUDIO,
            sequence=self._next_seq(),
            timestamp=timestamp,
            ssrc=self.ssrc,
            marker=marker,
        )
        pkt = header.encode() + pcm_be
        out = [pkt]
        self._protect(pkt, out, close_group=False)
        return out
