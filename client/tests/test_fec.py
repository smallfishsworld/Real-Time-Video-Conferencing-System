from __future__ import annotations

import random

import pytest

from confclient.fec import FecDecoder, FecEncoder, recover
from confclient.protocol import (
    MAX_FEC_GROUP,
    MAX_PROTECTED_BODY,
    RTP_HEADER_SIZE,
    ProtocolError,
    RtpHeader,
)

SSRC = 0x5EED


def _packets(rng: random.Random, n: int, seq0: int) -> list[bytes]:
    """n consecutive RTP packets with random flags, CSRC counts, timestamps and sizes."""
    out = []
    for i in range(n):
        csrc = tuple(rng.getrandbits(32) for _ in range(rng.randrange(4)))
        header = RtpHeader(
            payload_type=rng.choice((96, 97)),
            sequence=(seq0 + i) & 0xFFFF,
            timestamp=rng.getrandbits(32),
            ssrc=SSRC,
            marker=rng.random() < 0.3,
            csrc=csrc,
        ).encode()
        size = rng.randrange(1, MAX_PROTECTED_BODY - 4 * len(csrc) + 1)
        out.append(header + rng.randbytes(size))
    return out


def _parity(packets: list[bytes], seq: int) -> bytes:
    enc = FecEncoder()
    for p in packets:
        enc.add(p)
    return enc.flush(SSRC, seq)


@pytest.mark.parametrize("seed", range(20))
def test_any_single_loss_is_recovered_bit_exact(seed: int) -> None:
    rng = random.Random(seed)
    k = rng.randrange(1, 9)
    seq0 = rng.choice((0, 65533, rng.randrange(65536)))  # include sequence wrap-around
    packets = _packets(rng, k, seq0)
    parity = _parity(packets, (seq0 + k) & 0xFFFF)
    for i, missing in enumerate(packets):
        seq = (seq0 + i) & 0xFFFF
        assert recover(parity, packets[:i] + packets[i + 1 :], seq) == missing


def test_parity_is_as_long_as_the_longest_member() -> None:
    rng = random.Random(1)
    packets = _packets(rng, 3, 10)
    parity = _parity(packets, 13)
    longest = max(len(p) - RTP_HEADER_SIZE for p in packets)
    assert len(parity) == RTP_HEADER_SIZE + 12 + longest


def test_encoder_rejects_bad_groups() -> None:
    enc = FecEncoder()
    with pytest.raises(ProtocolError):
        enc.flush(SSRC, 0)  # empty
    rng = random.Random(2)
    a, b = _packets(rng, 2, 100)
    enc.add(a)
    with pytest.raises(ProtocolError):
        enc.add(a)  # not consecutive
    enc = FecEncoder()
    for p in _packets(rng, MAX_FEC_GROUP, 0):
        enc.add(p)
    with pytest.raises(ProtocolError):
        enc.add(_packets(rng, 1, MAX_FEC_GROUP)[0])
    too_big = RtpHeader(payload_type=96, sequence=0, timestamp=0, ssrc=SSRC).encode()
    with pytest.raises(ProtocolError):
        FecEncoder().add(too_big + bytes(MAX_PROTECTED_BODY + 1))
    del b


def test_recover_rejects_groups_without_exactly_one_loss() -> None:
    packets = _packets(random.Random(3), 4, 0)
    parity = _parity(packets, 4)
    with pytest.raises(ProtocolError):
        recover(parity, packets[:2], 2)  # two missing
    with pytest.raises(ProtocolError):
        recover(parity, packets[1:], 9)  # 9 is not in the group
    with pytest.raises(ProtocolError):
        recover(packets[0], packets[1:], 0)  # not a parity packet


def test_decoder_parity_after_media() -> None:
    packets = _packets(random.Random(4), 4, 500)
    parity = _parity(packets, 504)
    dec = FecDecoder()
    for i, p in enumerate(packets):
        if i != 2:
            assert dec.on_media(500 + i, p) == []
    assert dec.on_parity(parity) == [packets[2]]
    assert dec.recovered == 1
    assert dec.on_media(502, packets[2]) == []  # the late original is a duplicate


def test_decoder_parity_before_media() -> None:
    packets = _packets(random.Random(5), 3, 65535)
    parity = _parity(packets, 2)
    dec = FecDecoder()
    assert dec.on_parity(parity) == []
    assert dec.on_media(65535, packets[0]) == []
    assert dec.on_media(1, packets[2]) == [packets[1]]  # completes the group: rebuilds seq 0


def test_decoder_needs_all_but_one() -> None:
    packets = _packets(random.Random(6), 4, 0)
    parity = _parity(packets, 4)
    dec = FecDecoder()
    dec.on_media(0, packets[0])
    dec.on_media(3, packets[3])
    assert dec.on_parity(parity) == []  # two missing: nothing yet
    assert dec.on_media(1, packets[1]) == [packets[2]]  # now only one is missing
    assert dec.recovered == 1


def test_decoder_discards_parity_of_complete_groups() -> None:
    packets = _packets(random.Random(7), 2, 0)
    dec = FecDecoder()
    for i, p in enumerate(packets):
        dec.on_media(i, p)
    assert dec.on_parity(_parity(packets, 2)) == []
    assert dec.recovered == 0


def test_decoder_history_is_bounded() -> None:
    dec = FecDecoder(history=8)
    packets = _packets(random.Random(8), 20, 0)
    parity = _parity(packets[:2], 99)
    for i, p in enumerate(packets):
        if i != 1:
            dec.on_media(i, p)
    # Packet 0 has been evicted, so the group of packets 0 and 1 can no longer be repaired.
    assert dec.on_parity(parity) == []
