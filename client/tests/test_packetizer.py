from __future__ import annotations

import itertools
import os

import pytest

from confclient.packetizer import AudioPacketizer, VideoPacketizer
from confclient.protocol import (
    AUDIO_FRAME_BYTES,
    MAX_FRAGMENT_PAYLOAD,
    MAX_FRAME_BYTES,
    MAX_SEND_DATAGRAM,
    PT_AUDIO,
    PT_FEC,
    PT_VIDEO,
    ProtocolError,
    parse_fec,
    parse_rtp,
    parse_video,
    seq_diff,
)


def _split(packets: list[bytes]) -> tuple[list[bytes], list[bytes]]:
    media = [p for p in packets if parse_rtp(p)[0].payload_type != PT_FEC]
    parity = [p for p in packets if parse_rtp(p)[0].payload_type == PT_FEC]
    return media, parity


@pytest.mark.parametrize(("size", "k"), [(1, 4), (1200, 4), (4800, 4), (5000, 4), (31_000, 4), (31_000, 0)])
def test_video_fragments_and_parity_layout(size: int, k: int) -> None:
    jpeg = os.urandom(size)
    pz = VideoPacketizer(ssrc=0xAB, fec_k=k)
    frame_id, packets = pz.packetize(jpeg, timestamp=90_000)
    media, parity = _split(packets)
    count = -(-size // MAX_FRAGMENT_PAYLOAD)
    assert len(media) == count
    assert len(parity) == (0 if k == 0 else -(-count // k))
    assert all(len(p) <= MAX_SEND_DATAGRAM for p in packets)

    # One sequence space for media and parity, without gaps.
    seqs = [parse_rtp(p)[0].sequence for p in packets]
    assert all(seq_diff(b, a) == 1 for a, b in itertools.pairwise(seqs))

    chunks = []
    for i, p in enumerate(media):
        header, payload = parse_rtp(p)
        assert header.payload_type == PT_VIDEO
        assert header.ssrc == 0xAB
        assert header.timestamp == 90_000
        assert header.marker == (i == count - 1)  # marker on the last fragment only
        vh, frag = parse_video(payload)
        assert (vh.frame_id, vh.index, vh.count) == (frame_id, i, count)
        chunks.append(bytes(frag))
    assert b"".join(chunks) == jpeg

    # Each parity packet covers the media packets just before it, within this frame.
    covered: list[int] = []
    for p in parity:
        fec, _ = parse_fec(parse_rtp(p)[1])
        assert 1 <= fec.count <= max(k, 1)
        covered += fec.sequences()
    if k:
        assert covered == [parse_rtp(p)[0].sequence for p in media]


def test_video_frame_ids_and_sequences_continue_across_frames() -> None:
    pz = VideoPacketizer(ssrc=1, fec_k=4)
    pz.frame_id = 0xFFFFFFFF
    pz.seq = 0xFFFE
    f1, p1 = pz.packetize(b"x" * 3000, 0)
    f2, p2 = pz.packetize(b"y" * 3000, 6000)
    assert (f1, f2) == (0xFFFFFFFF, 0)
    last = parse_rtp(p1[-1])[0].sequence
    first = parse_rtp(p2[0])[0].sequence
    assert seq_diff(first, last) == 1
    # FEC groups never span two frames.
    for p in p2:
        if parse_rtp(p)[0].payload_type == PT_FEC:
            fec, _ = parse_fec(parse_rtp(p)[1])
            assert all(seq_diff(s, first) >= 0 for s in fec.sequences())


def test_video_size_limits() -> None:
    pz = VideoPacketizer(ssrc=1)
    with pytest.raises(ProtocolError):
        pz.packetize(b"", 0)
    with pytest.raises(ProtocolError):
        pz.packetize(bytes(MAX_FRAME_BYTES + 1), 0)
    _fid, packets = pz.packetize(bytes(MAX_FRAME_BYTES), 0)
    assert len(_split(packets)[0]) == 1024


def test_audio_packets_and_parity() -> None:
    pz = AudioPacketizer(ssrc=7, fec_k=2)
    pcm = bytes(range(256)) * 2 + bytes(128)
    assert len(pcm) == AUDIO_FRAME_BYTES
    first = pz.packetize(pcm, 320, marker=True)
    second = pz.packetize(pcm, 640)
    assert len(first) == 1
    assert len(second) == 2
    h1, body = parse_rtp(first[0])
    assert (h1.payload_type, h1.timestamp, h1.marker) == (PT_AUDIO, 320, True)
    assert bytes(body) == pcm
    fec, _ = parse_fec(parse_rtp(second[1])[1])
    assert fec.sequences() == [h1.sequence, parse_rtp(second[0])[0].sequence]
    assert AudioPacketizer(ssrc=7, fec_k=0).packetize(pcm, 0)[0][1] & 0x7F == PT_AUDIO
    with pytest.raises(ProtocolError):
        pz.packetize(pcm[:-2], 960)
    with pytest.raises(ValueError, match="fec_k"):
        AudioPacketizer(ssrc=7, fec_k=-1)
