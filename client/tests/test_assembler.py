"""Frame reassembly under loss, reordering and duplication."""

from __future__ import annotations

import os
import random

from confclient.assembler import FrameAssembler, VideoFrame
from confclient.fec import FecDecoder
from confclient.packetizer import VideoPacketizer
from confclient.protocol import PT_FEC, PT_VIDEO, parse_rtp, peek_ssrc


def _frames(n: int, size: int = 6000, fec_k: int = 0, seed: int = 0) -> list[tuple[bytes, list[bytes]]]:
    rng = random.Random(seed)
    pz = VideoPacketizer(ssrc=42, fec_k=fec_k)
    out = []
    for i in range(n):
        jpeg = rng.randbytes(size)
        _fid, pkts = pz.packetize(jpeg, i * 6000)
        out.append((jpeg, pkts))
    return out


def _push(asm: FrameAssembler, pkt: bytes, now: float = 0.0) -> VideoFrame | None:
    header, payload = parse_rtp(pkt)
    return asm.push(header, payload, now)


def test_in_order_frame_completes_on_last_fragment() -> None:
    ((jpeg, pkts),) = _frames(1)
    asm = FrameAssembler()
    results = [_push(asm, p, now=i * 0.001) for i, p in enumerate(pkts)]
    assert results[:-1] == [None] * (len(pkts) - 1)
    frame = results[-1]
    assert frame is not None
    assert frame.data == jpeg
    assert frame.ssrc == 42
    assert frame.first_arrival == 0.0
    assert frame.completed == (len(pkts) - 1) * 0.001
    assert asm.pending() == 0


def test_reordered_fragments() -> None:
    ((jpeg, pkts),) = _frames(1, size=20_000)
    random.Random(1).shuffle(pkts)
    asm = FrameAssembler()
    done = [f for f in (_push(asm, p) for p in pkts) if f is not None]
    assert [f.data for f in done] == [jpeg]


def test_duplicates_are_ignored_before_and_after_completion() -> None:
    ((jpeg, pkts),) = _frames(1)
    asm = FrameAssembler()
    assert _push(asm, pkts[0]) is None
    assert _push(asm, pkts[0]) is None  # duplicate while pending
    done = [f for f in (_push(asm, p) for p in pkts[1:]) if f is not None]
    assert [f.data for f in done] == [jpeg]
    assert _push(asm, pkts[2]) is None  # duplicate after the frame was emitted
    assert asm.stats.duplicates == 2
    assert asm.stats.completed == 1


def test_lost_fragment_expires_after_deadline() -> None:
    ((_jpeg, pkts),) = _frames(1)
    asm = FrameAssembler(deadline=0.3)
    for p in pkts[:-1]:
        assert _push(asm, p, now=10.0) is None
    assert asm.expire(10.2) == 0
    assert asm.expire(10.31) == 1
    assert asm.pending() == 0
    assert asm.stats.expired == 1


def test_newest_wins() -> None:
    (j1, p1), (j2, p2) = _frames(2)
    asm = FrameAssembler()
    for p in p1[:-1]:  # frame 1 is missing its last fragment
        _push(asm, p)
    done = [f for f in (_push(asm, p) for p in p2) if f is not None]
    assert [f.data for f in done] == [j2]
    assert asm.stats.superseded == 1  # the incomplete older frame was dropped
    assert _push(asm, p1[-1]) is None  # completing it now would show an older picture
    assert asm.pending() == 0
    del j1


def test_frame_ids_wrap_around() -> None:
    pz = VideoPacketizer(ssrc=1, fec_k=0)
    pz.frame_id = 0xFFFFFFFF
    asm = FrameAssembler()
    ids = []
    for i in range(3):
        _fid, pkts = pz.packetize(os.urandom(2000), i)
        for p in pkts:
            f = _push(asm, p)
            if f is not None:
                ids.append(f.frame_id)
    assert ids == [0xFFFFFFFF, 0, 1]


def test_pending_frames_are_bounded() -> None:
    frames = _frames(40)
    asm = FrameAssembler(max_pending=32)
    for i, (_jpeg, pkts) in enumerate(frames):
        _push(asm, pkts[0], now=float(i))
    assert asm.pending() == 32
    assert asm.stats.expired == 8


def test_malformed_payload_is_counted() -> None:
    ((_jpeg, pkts),) = _frames(1)
    header, _payload = parse_rtp(pkts[0])
    asm = FrameAssembler()
    assert asm.push(header, memoryview(b"\x00\x00"), 0.0) is None
    assert asm.stats.malformed == 1


def test_fec_repairs_one_loss_per_group() -> None:
    """Drop one packet of every FEC group on the way; every frame still completes."""
    frames = _frames(10, size=31_000, fec_k=4, seed=3)
    rng = random.Random(4)
    asm = FrameAssembler()
    dec = FecDecoder()
    done = []
    for _jpeg, pkts in frames:
        group: list[bytes] = []
        for p in pkts:
            group.append(p)
            if p[1] & 0x7F == PT_FEC:
                del group[rng.randrange(len(group) - 1)]  # lose one media packet of the group
                for q in group:
                    _ssrc, pt, seq = peek_ssrc(q)
                    repaired = dec.on_parity(q) if pt == PT_FEC else dec.on_media(seq, q)
                    media = [q] if pt == PT_VIDEO else []
                    for m in media + repaired:
                        f = _push(asm, m)
                        if f is not None:
                            done.append(f.data)
                group = []
    assert done == [jpeg for jpeg, _ in frames]
    assert dec.recovered == sum(1 for _, pkts in frames for p in pkts if p[1] & 0x7F == PT_FEC)
