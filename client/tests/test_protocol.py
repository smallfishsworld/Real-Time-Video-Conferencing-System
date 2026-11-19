"""The Python codec against the shared golden vectors (protocol-testdata/vectors.json),
which the Go server is tested against too."""

from __future__ import annotations

import asyncio
import struct
from collections.abc import Callable
from typing import Any

import pytest

from confclient.fec import FecEncoder, recover
from confclient.protocol import (
    MAX_FRAGMENT_PAYLOAD,
    MAX_SEND_DATAGRAM,
    FrameDecoder,
    ProtocolError,
    RtpHeader,
    VideoHeader,
    build_register,
    build_register_ack,
    encode_frame,
    fragment_count,
    is_rtp,
    parse_addr,
    parse_fec,
    parse_register,
    parse_register_ack,
    parse_rtp,
    parse_video,
    peek_ssrc,
    read_frame,
    seq_diff,
    u32_newer,
)

from .conftest import VECTORS


def _name(v: dict[str, Any]) -> str:
    return f"{v.get('kind', '')}-{v['name']}".lstrip("-")


@pytest.mark.parametrize("vec", VECTORS["rtp"], ids=_name)
def test_rtp_vectors(vec: dict[str, Any]) -> None:
    raw = bytes.fromhex(vec["hex"])
    header, payload = parse_rtp(raw)
    want = vec["header"]
    assert header.payload_type == want["pt"]
    assert header.marker == want["marker"]
    assert header.sequence == want["seq"]
    assert header.timestamp == want["ts"]
    assert header.ssrc == want["ssrc"]
    assert list(header.csrc) == want["csrc"]
    assert is_rtp(raw)
    assert peek_ssrc(raw) == (want["ssrc"], want["pt"], want["seq"])

    body = bytes(payload)
    video = vec.get("video")
    if video is not None:
        vh, frag = parse_video(payload)
        assert (vh.frame_id, vh.index, vh.count) == (video["frame_id"], video["index"], video["count"])
        body = bytes(frag)
    assert body.hex() == vec["payload_hex"]

    if not vec.get("decode_only"):
        rebuilt = RtpHeader(
            payload_type=want["pt"],
            sequence=want["seq"],
            timestamp=want["ts"],
            ssrc=want["ssrc"],
            marker=want["marker"],
            csrc=tuple(want["csrc"]),
        ).encode()
        if video is not None:
            rebuilt += VideoHeader(video["frame_id"], video["index"], video["count"]).encode()
        assert rebuilt + bytes.fromhex(vec["payload_hex"]) == raw


@pytest.mark.parametrize("vec", VECTORS["fec"], ids=_name)
def test_fec_vectors_encode_and_recover_every_member(vec: dict[str, Any]) -> None:
    packets = [bytes.fromhex(h) for h in vec["packets_hex"]]
    enc = FecEncoder()
    for p in packets:
        enc.add(p)
    (ssrc,) = struct.unpack_from("!I", packets[0], 8)
    parity = enc.flush(ssrc, vec["parity_seq"])
    assert parity.hex() == vec["parity_hex"]
    _hdr, payload = parse_rtp(parity)
    fec, _ = parse_fec(payload)
    assert fec.count == len(packets)
    for i, missing in enumerate(packets):
        (seq,) = struct.unpack_from("!H", missing, 2)
        assert fec.covers(seq)
        assert recover(parity, packets[:i] + packets[i + 1 :], seq) == missing


def test_register_vectors() -> None:
    by_name = {v["name"]: v for v in VECTORS["register"]}
    reg = by_name["register"]
    token = bytes.fromhex(reg["token"])
    assert build_register(reg["txid"], token).hex() == reg["hex"]
    assert parse_register(bytes.fromhex(reg["hex"])) == (reg["txid"], token)
    for name in ("register_ack_ipv4", "register_ack_ipv6"):
        v = by_name[name]
        host, port = parse_addr(v["addr"])
        assert build_register_ack(v["txid"], host, port).hex() == v["hex"]
        assert parse_register_ack(bytes.fromhex(v["hex"])) == (v["txid"], (host, port))


@pytest.mark.parametrize("vec", VECTORS["control"], ids=_name)
def test_control_vectors(vec: dict[str, Any]) -> None:
    raw = bytes.fromhex(vec["hex"])
    assert encode_frame(vec["message"]) == raw
    dec = FrameDecoder()
    got: list[dict[str, Any]] = []
    for i in range(len(raw)):  # one byte per read: framing must not depend on read sizes
        got += dec.feed(raw[i : i + 1])
    assert got == [vec["message"]]


def test_control_stream_of_all_vectors_in_one_read() -> None:
    stream = b"".join(bytes.fromhex(v["hex"]) for v in VECTORS["control"])
    assert FrameDecoder().feed(stream) == [v["message"] for v in VECTORS["control"]]


def test_read_frame_from_asyncio_stream() -> None:
    async def scenario() -> list[dict[str, Any]]:
        reader = asyncio.StreamReader()
        for v in VECTORS["control"]:
            reader.feed_data(bytes.fromhex(v["hex"]))
        reader.feed_eof()
        return [await read_frame(reader) for _ in VECTORS["control"]]

    assert asyncio.run(scenario()) == [v["message"] for v in VECTORS["control"]]


_PARSERS: dict[str, Callable[[bytes], object]] = {
    "rtp": parse_rtp,
    "video": parse_video,
    "fec": parse_fec,
    "register": parse_register,
    "control": lambda b: FrameDecoder().feed(b),
}


@pytest.mark.parametrize("vec", VECTORS["invalid"], ids=_name)
def test_invalid_vectors_are_rejected(vec: dict[str, Any]) -> None:
    with pytest.raises(ProtocolError):
        _PARSERS[vec["kind"]](bytes.fromhex(vec["hex"]))


def test_sequence_arithmetic_wraps() -> None:
    assert seq_diff(1, 65535) == 2
    assert seq_diff(65535, 1) == -2
    assert seq_diff(7, 7) == 0
    assert u32_newer(0, 0xFFFFFFFF)
    assert not u32_newer(0xFFFFFFFF, 0)
    assert not u32_newer(5, 5)


def test_fragment_count_and_datagram_budget() -> None:
    assert fragment_count(1) == 1
    assert fragment_count(MAX_FRAGMENT_PAYLOAD) == 1
    assert fragment_count(MAX_FRAGMENT_PAYLOAD + 1) == 2
    # The largest packet (an FEC parity packet over full fragments) fits a 1280-byte
    # IPv6 minimum MTU with IPv6 and UDP headers: 1232 + 40 + 8 = 1280.
    assert MAX_SEND_DATAGRAM == 1232


def test_encoder_rejects_invalid_fields() -> None:
    with pytest.raises(ProtocolError):
        RtpHeader(payload_type=128, sequence=0, timestamp=0, ssrc=0).encode()
    with pytest.raises(ProtocolError):
        RtpHeader(payload_type=96, sequence=0, timestamp=0, ssrc=0, csrc=tuple(range(16))).encode()
    with pytest.raises(ProtocolError):
        VideoHeader(1, 3, 3).encode()
    with pytest.raises(ProtocolError):
        build_register(1, b"short")
    with pytest.raises(ProtocolError):
        encode_frame({"type": "chat", "text": "x" * (64 * 1024)})


def test_parse_addr() -> None:
    assert parse_addr("127.0.0.1:6100") == ("127.0.0.1", 6100)
    assert parse_addr("[::1]:5000") == ("::1", 5000)
    with pytest.raises(ValueError, match="bad address"):
        parse_addr("no-port")
    with pytest.raises(ValueError, match="does not appear to be an IPv4 or IPv6 address"):
        parse_addr("example.com:80")
