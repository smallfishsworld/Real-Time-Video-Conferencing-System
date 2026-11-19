"""Generate protocol-testdata/vectors.json from docs/PROTOCOL.md.

This is a deliberately independent reference encoder: it builds every packet with
struct.pack straight from the byte layouts in the protocol document and shares no code
with the Go server or the Python client. Both implementations are tested against its
output. Run it with `python3 protocol-testdata/generate_vectors.py`; the output is
deterministic.
"""

from __future__ import annotations

import json
import struct
from pathlib import Path


def rtp(
    pt: int,
    seq: int,
    ts: int,
    ssrc: int,
    *,
    marker: bool = False,
    csrc: tuple[int, ...] = (),
    padding: bool = False,
    extension: bool = False,
) -> bytes:
    b0 = (2 << 6) | (0x20 if padding else 0) | (0x10 if extension else 0) | len(csrc)
    b1 = (0x80 if marker else 0) | pt
    out = struct.pack("!BBHII", b0, b1, seq, ts, ssrc)
    for c in csrc:
        out += struct.pack("!I", c)
    return out


def video_header(frame_id: int, index: int, count: int) -> bytes:
    return struct.pack("!IHH", frame_id, index, count)


def xor_parity(packets: list[bytes], ssrc: int, seq: int) -> bytes:
    bodies = [p[12:] for p in packets]
    longest = max(len(b) for b in bodies)
    b0 = b1 = length = ts = 0
    parity = bytearray(longest)
    for p, body in zip(packets, bodies):
        b0 ^= p[0] & 0x3F
        b1 ^= p[1]
        length ^= len(body)
        ts ^= struct.unpack("!I", p[4:8])[0]
        for i, byte in enumerate(body):
            parity[i] ^= byte
    sn_base = struct.unpack("!H", packets[0][2:4])[0]
    last_ts = struct.unpack("!I", packets[-1][4:8])[0]
    header = rtp(127, seq, last_ts, ssrc)
    fec = struct.pack("!HBBBBHI", sn_base, len(packets), 0, b0, b1, length, ts)
    return header + fec + bytes(parity)


def nack(sender: int, media: int, items: list[tuple[int, int]]) -> bytes:
    """RTCP generic NACK (RFC 4585): header, two SSRCs, then (PID, BLP) pairs."""
    head = struct.pack("!BBHII", 0x80 | 1, 205, 2 + len(items), sender, media)
    return head + b"".join(struct.pack("!HH", pid, blp) for pid, blp in items)


def frame(body: dict) -> bytes:
    js = json.dumps(body, separators=(",", ":"), ensure_ascii=False).encode()
    return struct.pack("!I", len(js)) + js


def audio_samples(n: int, mul: int) -> bytes:
    return b"".join(struct.pack("!h", ((i * mul) % 65536) - 32768) for i in range(n))


def main() -> None:
    vectors: dict[str, list[dict]] = {"rtp": [], "fec": [], "register": [], "control": [], "invalid": []}

    jpeg = bytes(range(1, 9))
    v = rtp(96, 0x1234, 0xDEADBEEF, 0x11223344) + video_header(7, 2, 5) + jpeg
    vectors["rtp"].append(
        {
            "name": "video_fragment",
            "hex": v.hex(),
            "header": {"marker": False, "pt": 96, "seq": 0x1234, "ts": 0xDEADBEEF, "ssrc": 0x11223344, "csrc": []},
            "video": {"frame_id": 7, "index": 2, "count": 5},
            "payload_hex": jpeg.hex(),
        }
    )
    v = rtp(96, 0xFFFF, 90000, 0xCAFEBABE, marker=True) + video_header(0xFFFFFFFF, 4, 5) + b"\xff\xd9"
    vectors["rtp"].append(
        {
            "name": "video_last_fragment_marker",
            "hex": v.hex(),
            "header": {"marker": True, "pt": 96, "seq": 0xFFFF, "ts": 90000, "ssrc": 0xCAFEBABE, "csrc": []},
            "video": {"frame_id": 0xFFFFFFFF, "index": 4, "count": 5},
            "payload_hex": "ffd9",
        }
    )
    pcm = audio_samples(320, 37)
    a = rtp(97, 65535, 320 * 3, 0x0BADF00D) + pcm
    vectors["rtp"].append(
        {
            "name": "audio_frame",
            "hex": a.hex(),
            "header": {"marker": False, "pt": 97, "seq": 65535, "ts": 960, "ssrc": 0x0BADF00D, "csrc": []},
            "payload_hex": pcm.hex(),
        }
    )
    pcm2 = audio_samples(320, 101)
    m = rtp(97, 7, 640, 0x99999999, marker=True, csrc=(0xA, 0xB)) + pcm2
    vectors["rtp"].append(
        {
            "name": "mixed_audio_with_csrc",
            "hex": m.hex(),
            "header": {"marker": True, "pt": 97, "seq": 7, "ts": 640, "ssrc": 0x99999999, "csrc": [0xA, 0xB]},
            "payload_hex": pcm2.hex(),
        }
    )
    ext = struct.pack("!HH", 0xBEDE, 1) + b"\x10\xaa\x00\x00"
    body = b"hello"
    pad = b"\x00\x00\x03"
    p = rtp(96, 1, 2, 3, padding=True, extension=True) + ext + body + pad
    vectors["rtp"].append(
        {
            "name": "decode_only_padding_and_extension",
            "decode_only": True,
            "hex": p.hex(),
            "header": {"marker": False, "pt": 96, "seq": 1, "ts": 2, "ssrc": 3, "csrc": []},
            "payload_hex": body.hex(),
        }
    )

    ssrc = 0x01020304
    group = [
        rtp(96, 100, 9000, ssrc) + video_header(3, 0, 3) + bytes(range(10, 40)),
        rtp(96, 101, 9000, ssrc) + video_header(3, 1, 3) + bytes(range(50, 62)),
        rtp(96, 102, 9000, ssrc, marker=True) + video_header(3, 2, 3) + bytes(range(70, 75)),
    ]
    vectors["fec"].append(
        {
            "name": "video_group_k3",
            "packets_hex": [g.hex() for g in group],
            "parity_seq": 103,
            "parity_hex": xor_parity(group, ssrc, 103).hex(),
        }
    )
    audio_group = [
        rtp(97, 65534, 1000, 0xABCDEF01, marker=True) + audio_samples(320, 3),
        rtp(97, 65535, 1320, 0xABCDEF01) + audio_samples(320, 5),
    ]
    vectors["fec"].append(
        {
            "name": "audio_group_k2_seq_wrap",
            "packets_hex": [g.hex() for g in audio_group],
            "parity_seq": 0,
            "parity_hex": xor_parity(audio_group, 0xABCDEF01, 0).hex(),
        }
    )
    mixed_group = [
        rtp(97, 10, 3200, 0x55, csrc=(1, 2, 3)) + audio_samples(320, 7),
        rtp(97, 11, 3520, 0x55, csrc=(1,)) + audio_samples(320, 11),
    ]
    vectors["fec"].append(
        {
            "name": "mixed_audio_group_different_csrc_counts",
            "packets_hex": [g.hex() for g in mixed_group],
            "parity_seq": 12,
            "parity_hex": xor_parity(mixed_group, 0x55, 12).hex(),
        }
    )

    token = bytes(range(0xA0, 0xB0))
    nonce = bytes(range(0xC0, 0xC8))
    reg = struct.pack("!BBBBQ", 1, 2, 0, 0, 0x0102030405060708) + token + bytes(8)
    vectors["register"].append(
        {"name": "register", "hex": reg.hex(), "txid": 0x0102030405060708, "token": token.hex(), "nonce": None}
    )
    reg_nonce = struct.pack("!BBBBQ", 1, 2, 1, 0, 0x1112131415161718) + token + nonce
    vectors["register"].append(
        {
            "name": "register_echoing_nonce",
            "hex": reg_nonce.hex(),
            "txid": 0x1112131415161718,
            "token": token.hex(),
            "nonce": nonce.hex(),
        }
    )
    ack4 = struct.pack("!BBBBQH", 2, 2, 4, 0, 42, 50000) + bytes([127, 0, 0, 2]) + bytes(8)
    vectors["register"].append(
        {
            "name": "register_ack_ipv4",
            "hex": ack4.hex(),
            "txid": 42,
            "addr": "127.0.0.2:50000",
            "challenge": False,
            "nonce": bytes(8).hex(),
        }
    )
    challenge4 = struct.pack("!BBBBQH", 2, 2, 4, 1, 43, 50001) + bytes([127, 0, 0, 2]) + nonce
    vectors["register"].append(
        {
            "name": "register_challenge_ipv4",
            "hex": challenge4.hex(),
            "txid": 43,
            "addr": "127.0.0.2:50001",
            "challenge": True,
            "nonce": nonce.hex(),
        }
    )
    ip6 = bytes.fromhex("20010db8000000000000000000000001")
    ack6 = struct.pack("!BBBBQH", 2, 2, 6, 0, 7, 6100) + ip6 + bytes(8)
    vectors["register"].append(
        {
            "name": "register_ack_ipv6",
            "hex": ack6.hex(),
            "txid": 7,
            "addr": "[2001:db8::1]:6100",
            "challenge": False,
            "nonce": bytes(8).hex(),
        }
    )

    vectors["nack"] = [
        {
            "name": "single",
            "hex": nack(0x0A0B0C0D, 0x01020304, [(100, 0)]).hex(),
            "sender_ssrc": 0x0A0B0C0D,
            "media_ssrc": 0x01020304,
            "lost": [100],
        },
        {
            "name": "bitmask_and_wrap",
            "hex": nack(7, 8, [(65534, 0b1000000000000101), (300, 0b10)]).hex(),
            "sender_ssrc": 7,
            "media_ssrc": 8,
            "lost": [65534, 65535, 1, 14, 300, 302],
        },
    ]

    # (sender, message). The first six keep their historical names (the message type);
    # the rest cover every other message type and the optional fields.
    messages = [
        ("client", {"type": "hello", "name": "alice", "version": 1, "seq": 1}),
        ("client", {"type": "join_room", "room_id": 3, "seq": 2}),
        ("server", {
            "type": "joined",
            "seq": 2,
            "room_id": 3,
            "title": "standup",
            "host": False,
            "participant_id": 9,
            "audio_ssrc": 305419896,
            "video_ssrc": 2596069104,
            "mixer_ssrc": 4022250974,
            "token": token.hex(),
            "media_port": 6100,
            "max_participants": 16,
        }),
        ("server", {
            "type": "chat",
            "room_id": 3,
            "from_id": 9,
            "from_name": "Zoë",
            "text": "hi 你好",
            "server_time_ms": 1700000000123,
        }),
        ("server", {
            "type": "mode",
            "room_id": 3,
            "mode": "p2p",
            "peer": {"participant_id": 4, "audio_ssrc": 1, "video_ssrc": 2, "addr": "127.0.0.1:40000"},
        }),
        ("server", {"type": "error", "seq": 5, "code": "room_full", "message": "room 3 is full"}),
    ]
    named = [(msg["type"], sender, msg) for sender, msg in messages]
    named += [
        ("welcome", "server", {"type": "welcome", "seq": 1, "client_id": 42, "version": 1,
                               "server_time_ms": 1700000000000, "media_port": 6100}),
        ("list_rooms", "client", {"type": "list_rooms", "seq": 3}),
        ("room_list", "server", {"type": "room_list", "seq": 3, "rooms": [
            {"room_id": 3, "title": "standup <&>", "host_name": "Zoë", "participants": 2,
             "max_participants": 16, "mode": "p2p", "created_ms": 1700000000001},
        ]}),
        ("room_list_truncated", "server", {"type": "room_list", "seq": 4, "rooms": [], "truncated": True,
                                           "total": 300}),
        ("create_room", "client", {"type": "create_room", "title": "standup", "seq": 6}),
        ("leave_room", "client", {"type": "leave_room", "seq": 7}),
        ("left", "server", {"type": "left", "seq": 7, "room_id": 3}),
        ("cancel_room", "client", {"type": "cancel_room", "seq": 8}),
        ("room_closed_reply", "server", {"type": "room_closed", "seq": 8, "room_id": 3, "reason": "cancelled"}),
        ("room_closed_event", "server", {"type": "room_closed", "room_id": 3, "reason": "server_shutdown"}),
        ("chat_request", "client", {"type": "chat", "text": "hi 你好", "seq": 9}),
        ("chat_own_copy", "server", {"type": "chat", "seq": 9, "room_id": 3, "from_id": 9, "from_name": "Zoë",
                                     "text": "hi 你好", "server_time_ms": 1700000000124}),
        ("media_state", "client", {"type": "media_state", "video": False, "audio": True, "seq": 10}),
        ("roster_own_copy", "server", {"type": "roster", "seq": 10, "room_id": 3, "host_id": 4, "participants": [
            {"participant_id": 4, "name": "bob", "audio_ssrc": 1, "video_ssrc": 2, "video": True,
             "audio": True, "registered": True},
            {"participant_id": 9, "name": "Zoë", "audio_ssrc": 305419896, "video_ssrc": 2596069104,
             "video": False, "audio": True, "registered": False},
        ]}),
        ("mode_relay", "server", {"type": "mode", "room_id": 3, "mode": "relay"}),
        ("ping", "client", {"type": "ping", "seq": 11}),
        ("pong", "server", {"type": "pong", "seq": 11, "server_time_ms": 1700000000125}),
    ]
    for name, sender, msg in named:
        vectors["control"].append({"name": name, "from": sender, "hex": frame(msg).hex(), "message": msg})

    # Whole frames as a sender packetizes them: fragments of at most 1200 bytes, and
    # with fec_k > 0 one parity packet after every fec_k fragments and after the last.
    vectors["frames"] = []
    for name, size, fec_k, seq in (("three_fragments_k2", 2500, 2, 65534), ("one_full_fragment_k4", 1200, 4, 10),
                                   ("four_fragments_no_fec", 3601, 0, 100), ("two_groups_k3", 4000, 3, 7)):
        data = bytes((i * 7 + 3) & 0xFF for i in range(size))
        count = -(-size // 1200)
        ssrc, frame_id, ts = 0x01020304, 77, 123456
        packets: list[bytes] = []
        group: list[bytes] = []
        for i in range(count):
            pkt = rtp(96, seq, ts, ssrc, marker=i == count - 1) + video_header(frame_id, i, count)
            pkt += data[i * 1200 : (i + 1) * 1200]
            seq = (seq + 1) & 0xFFFF
            packets.append(pkt)
            group.append(pkt)
            if fec_k and (len(group) == fec_k or i == count - 1):
                packets.append(xor_parity(group, ssrc, seq))
                seq = (seq + 1) & 0xFFFF
                group = []
        vectors["frames"].append({
            "name": name, "ssrc": ssrc, "first_seq": (seq - len(packets)) & 0xFFFF, "frame_id": frame_id,
            "timestamp": ts, "fec_k": fec_k, "size": size, "data": "bytes((i * 7 + 3) & 0xFF for i in range(size))",
            "packets_hex": [x.hex() for x in packets],
        })

    vectors["invalid"] = [
        {"kind": "rtp", "name": "short", "hex": "80600001"},
        {"kind": "rtp", "name": "version1", "hex": rtp(96, 1, 1, 1).hex().replace("80", "40", 1)},
        {"kind": "rtp", "name": "csrc_truncated", "hex": rtp(97, 1, 1, 1, csrc=(1, 2)).hex()[:-8]},
        {"kind": "rtp", "name": "padding_zero", "hex": (rtp(96, 1, 1, 1, padding=True) + b"ab\x00").hex()},
        {"kind": "rtp", "name": "padding_too_long", "hex": (rtp(96, 1, 1, 1, padding=True) + b"a\x09").hex()},
        {"kind": "video", "name": "count_zero", "hex": (video_header(1, 0, 0) + b"x").hex()},
        {"kind": "video", "name": "index_ge_count", "hex": (video_header(1, 3, 3) + b"x").hex()},
        {"kind": "video", "name": "count_too_large", "hex": (video_header(1, 0, 1025) + b"x").hex()},
        {"kind": "video", "name": "no_fragment_bytes", "hex": video_header(1, 0, 1).hex()},
        {"kind": "fec", "name": "count_zero", "hex": struct.pack("!HBBBBHI", 1, 0, 0, 0, 0, 0, 0).hex()},
        {"kind": "fec", "name": "count_too_large", "hex": struct.pack("!HBBBBHI", 1, 49, 0, 0, 0, 0, 0).hex()},
        {"kind": "register", "name": "short", "hex": reg[:28].hex()},
        {"kind": "register", "name": "version1", "hex": (reg[:1] + b"\x01" + reg[2:]).hex()},
        {"kind": "register", "name": "unknown_flag", "hex": (reg[:2] + b"\x02" + reg[3:]).hex()},
        {"kind": "register_ack", "name": "no_nonce", "hex": ack4[:18].hex()},
        {"kind": "register_ack", "name": "unknown_flag", "hex": (ack4[:3] + b"\x02" + ack4[4:]).hex()},
        {"kind": "nack", "name": "no_items", "hex": nack(1, 2, []).hex()},
        {"kind": "nack", "name": "length_beyond_packet", "hex": nack(1, 2, [(5, 0)]).hex()[:-8]},
        {"kind": "control", "name": "zero_length", "hex": "00000000"},
        {"kind": "control", "name": "too_large", "hex": "00010001"},
        {"kind": "control", "name": "no_type", "hex": frame({"name": "x"}).hex()},
    ]

    out = Path(__file__).with_name("vectors.json")
    out.write_text(json.dumps(vectors, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {out}")
    write_fuzz_seeds(vectors)


def go_bytes(b: bytes) -> str:
    return '[]byte("' + "".join(f"\\x{x:02x}" for x in b) + '")'


def write_fuzz_seeds(vectors: dict[str, list[dict]]) -> None:
    """Write the vectors as the committed seed corpus of the Go fuzz targets."""
    rtp_pkts = [bytes.fromhex(v["hex"]) for v in vectors["rtp"]]
    parities = [bytes.fromhex(v["parity_hex"]) for v in vectors["fec"]]
    invalid = {k: [bytes.fromhex(v["hex"]) for v in vectors["invalid"] if v["kind"] == k] for k in
               ("rtp", "video", "fec", "register", "register_ack", "control", "nack")}
    registers = [bytes.fromhex(v["hex"]) for v in vectors["register"] if "token" in v]
    acks = [bytes.fromhex(v["hex"]) for v in vectors["register"] if "addr" in v]
    seeds: dict[str, list[tuple[bytes, ...]]] = {
        "FuzzParseRTP": [(p,) for p in rtp_pkts + parities + invalid["rtp"]],
        "FuzzParseVideo": [(p[12:],) for p in rtp_pkts[:2]] + [(p,) for p in invalid["video"]],
        "FuzzParseFEC": [(p[12:],) for p in parities] + [(p,) for p in invalid["fec"]],
        "FuzzFECRecovery": [
            (bytes.fromhex(v["parity_hex"]), bytes.fromhex(v["packets_hex"][0])) for v in vectors["fec"]
        ],
        "FuzzParseRegister": [(p,) for p in registers + invalid["register"]],
        "FuzzParseRegisterAck": [(p,) for p in acks + invalid["register_ack"]],
        "FuzzControlFrame": [(bytes.fromhex(v["hex"]),) for v in vectors["control"]]
        + [(p,) for p in invalid["control"]],
        "FuzzParseNACK": [(bytes.fromhex(v["hex"]),) for v in vectors["nack"]] + [(p,) for p in invalid["nack"]],
    }
    root = Path(__file__).resolve().parent.parent / "server" / "internal" / "protocol" / "testdata" / "fuzz"
    for target, entries in seeds.items():
        d = root / target
        d.mkdir(parents=True, exist_ok=True)
        for old in d.glob("seed-*"):
            old.unlink()
        for i, args in enumerate(entries):
            text = "go test fuzz v1\n" + "".join(go_bytes(a) + "\n" for a in args)
            (d / f"seed-{i:02d}").write_text(text)
    print(f"wrote fuzz seed corpus under {root}")


if __name__ == "__main__":
    main()
