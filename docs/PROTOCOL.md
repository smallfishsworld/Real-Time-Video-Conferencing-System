# Wire protocol (control version 1, media registration version 2)

This document is the contract between the Go server (`server/`) and the Python client
(`client/`). Both sides are tested against the shared golden vectors in
[`protocol-testdata/vectors.json`](../protocol-testdata/vectors.json). All multi-byte
integers are big-endian (network byte order).

The system uses two transports:

| Plane   | Transport | Default port | Carries |
|---------|-----------|--------------|---------|
| Control | TCP       | 6100         | session, rooms, chat, roster, mode decisions |
| Media   | UDP       | 6100         | registration, RTP video/audio, FEC parity |

One UDP port serves every room. Rooms are separated by the server's routing table,
not by port numbers.

## 1. Control plane (TCP)

### 1.1 Framing

Each message is a 4-byte unsigned length `L` followed by `L` bytes of UTF-8 JSON:

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        length L (uint32)                      |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 L bytes: one JSON object ...                  |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

* `1 <= L <= 65536` (`MaxControlMessage`). A peer that receives a larger or zero length
  closes the connection; the server first sends an `error` with code `too_large`.
* The JSON value must be an object with a string field `type`. Unknown fields are
  ignored, so either side may add optional fields without a version bump.
* Readers must not assume that one `recv()` returns one message: a reader keeps a
  buffer and extracts complete frames.

### 1.2 Requests, replies and events

A client request may carry an integer `seq`. The server copies it into the direct reply
(success or `error`) so that the client can match replies to requests. Messages the
server sends on its own (events) carry no `seq`. For `chat` and `media_state` the reply
is the requester's own copy of the event the request causes (`chat` or `roster`): that
copy carries `seq`, and the copies other members get do not.

The first message on a connection must be `hello`. Until it has been accepted every
other request gets `error` with code `not_hello`.

### 1.3 Client to server

| `type`        | Fields | Reply |
|---------------|--------|-------|
| `hello`       | `name` (string, 1..32 characters, no control characters), `version` (int, must be 1) | `welcome` |
| `list_rooms`  | none | `room_list` |
| `create_room` | `title` (string, optional, at most 64 characters) | `joined` (the creator joins as host) |
| `join_room`   | `room_id` (int) | `joined` |
| `leave_room`  | none | `left` |
| `cancel_room` | none (host only) | `room_closed` with `reason: "cancelled"` |
| `chat`        | `text` (string, 1..2000 characters) | the `chat` event, sent to every member; the sender's copy carries `seq` |
| `media_state` | `video` (bool), `audio` (bool) | the `roster` event, sent to every member; the requester's copy carries `seq` |
| `ping`        | none | `pong` |

### 1.4 Server to client

| `type`        | Fields |
|---------------|--------|
| `welcome`     | `client_id` (int), `version` (int), `server_time_ms` (int, Unix milliseconds), `media_port` (int) |
| `room_list`   | `rooms`: list of `{room_id, title, host_name, participants, max_participants, mode, created_ms}` |
| `joined`      | `room_id`, `title`, `host` (bool), `participant_id`, `audio_ssrc`, `video_ssrc`, `mixer_ssrc` (ints), `token` (32 hex characters), `media_port` (int), `max_participants` (int) |
| `left`        | `room_id` |
| `roster`      | `room_id`, `host_id` (0 if the host has left), `participants`: list of `{participant_id, name, audio_ssrc, video_ssrc, video, audio, registered}` |
| `mode`        | `room_id`, `mode` (`"p2p"` or `"relay"`), `peer` (only for p2p: `{participant_id, audio_ssrc, video_ssrc, addr}` where `addr` is `"ip:port"` as observed by the server) |
| `chat`        | `room_id`, `from_id`, `from_name`, `text`, `server_time_ms` |
| `room_closed` | `room_id`, `reason` (`"cancelled"` or `"server_shutdown"`) |
| `pong`        | `server_time_ms` |
| `error`       | `code`, `message` |

The server stamps every chat message with the sender's name and its own clock
(`server_time_ms`); the client's clock is never trusted for timestamps.

Error codes: `bad_request`, `not_hello`, `unsupported_version`, `already_in_room`,
`not_in_room`, `room_not_found`, `room_full`, `room_closed`, `not_host`, `too_large`,
`rate_limited` (the connection sends requests faster than its budget, default 50/s),
`limit_reached` (server-wide room or client limit), `internal`.

### 1.5 Room life cycle

```
            create_room                join_room / leave_room
  (none) ───────────────▶ open ◀──────────────────────────────┐
                           │  │                               │
                           │  └───────────────────────────────┘
          cancel_room      │      last member leaves
          by the host      ▼      (or disconnects)
                         closed ◀───────────────────────────── open
```

* `create_room` creates a room and joins its creator as host in one step, so a room never
  exists without members.
* `leave_room`, or closing the TCP connection, removes a member. The room keeps running
  for the others. When the last member leaves, the room is closed.
* `cancel_room` is accepted only from the host. Every member, including the host,
  receives `room_closed` and is removed; the room id is never reused.
* The host role is not transferred: if the host leaves, `host_id` becomes 0 and nobody can
  cancel the room; it closes when it becomes empty (the course specification gives the
  cancel right to the creator only).
* `join_room` fails with `room_full` when the room has `max_participants` members
  (server flag, default 16).

### 1.6 Mode decision

The server decides the media mode of each room and announces it with `mode`:

* **p2p** when P2P is enabled on the server, the room has exactly two members and both have
  registered a UDP address. Each of the two receives the other's registered address.
  The server forwards nothing for the room.
* **relay** otherwise. Clients send one copy of their media to the server. The server
  forwards video packets to every other member unchanged (SFU) and sends each member one
  mixed audio stream that contains everyone except that member (mix-minus).

The server sends `mode` to all members whenever the decision or the peer address
changes. After a relay to p2p switch the server keeps relaying for a grace period
(1 s) so that packets already in flight on the old path are not lost; clients
likewise accept media from both the server and the peer for 2 s after any switch.

## 2. Media plane (UDP)

### 2.1 Demultiplexing

The first byte of a datagram decides how it is parsed (the same idea as RFC 7983):

| First byte  | Packet |
|-------------|--------|
| `0x01`      | `REGISTER` (client to server) |
| `0x02`      | `REGISTER_ACK` (server to client) |
| `0x81`, then `0xCD` (205) | RTCP generic NACK (client to server, section 2.9) |
| `0x80-0xBF` | RTP (version 2) |
| other       | dropped and counted as malformed |

The NACK row is checked first. RTP packets of this system never have 205 in their
second byte (payload types 96, 97 and 127, with or without the marker bit), so the two
cannot be confused (the same rule as RFC 5761).

Datagrams longer than 1500 bytes are dropped. Senders keep every datagram at or below
1232 bytes of UDP payload, so packets never need IP fragmentation on a 1500-byte MTU
path (IPv4 or IPv6).

### 2.2 REGISTER and REGISTER_ACK

After `joined`, the client sends `REGISTER` from the UDP socket it will use for media,
every 200 ms until it receives a final `REGISTER_ACK`, and then every 5 s as a
keepalive (this also keeps NAT bindings open).

The server binds an address only after a return-routability check, as STUN and ICE do,
so that a token holder cannot point a room's downstream at an address that never asked
for it:

1. A `REGISTER` from an address that is not the member's current binding (the first
   one, or one after a NAT rebinding) is answered with a challenge: a `REGISTER_ACK`
   with the challenge flag set and an 8-byte nonce. The nonce is an HMAC of the token,
   the address and a 30 s epoch under a key the server picks at startup; it is valid
   in its epoch and the next one. Nothing is bound yet.
2. The client sends `REGISTER` again from the same address, with the nonce flag set and
   the nonce echoed. The server queues the binding change; when it has taken effect it
   sends the final `REGISTER_ACK` (challenge flag clear). A later move goes through
   the same two steps.
3. A `REGISTER` from the bound address is a keepalive and is answered at once with a
   final `REGISTER_ACK`. A binding that gets no keepalive for 15 s
   (`-binding-timeout`) is dropped: the server stops sending to it, the roster shows
   the member as not registered, and the next `REGISTER` starts again at step 1.

A client should answer every challenge it receives, at any time, by echoing its nonce
from the socket it arrived on. The server ignores `REGISTER`s with unknown tokens, and
limits `REGISTER`s per member (10/s, burst 20), per source IP and port (20/s, burst
40; sources share 4096 slots by a keyed hash, so a collision only makes the limit
stricter, and one host with many sockets gets one budget per socket), and for the
whole server (`-max-registers-per-sec`, default 2000), and binding changes per member
(1/s, burst 4).

```
REGISTER (36 bytes)
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|  type = 0x01  |  version = 2  |     flags     | reserved (0)  |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 transaction id (64 bits, random)              |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                  session token (128 bits, from `joined`)      |
|                                                               |
|                                                               |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|          echoed nonce (64 bits; zero unless flags bit 0)      |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
flags: bit 0 = the packet echoes a challenge's nonce; other bits must be 0.

REGISTER_ACK (26 bytes for IPv4, 38 bytes for IPv6)
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|  type = 0x02  |  version = 2  | family (4|6)  |     flags     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 transaction id (echoed)                       |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|        observed port          |  observed address (4 or 16    |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+  bytes) ...                   |
|  ... nonce (64 bits; zero in a final ACK)                     |
flags: bit 0 = challenge (nothing bound yet; echo the nonce); other bits must be 0.
```

The ACK reports the address the server saw (like a STUN binding response), which the
client can log. Version 1 packets (28-byte `REGISTER`, no nonce) are rejected, so an
old client fails to register instead of taking a challenge for a binding. The check
stops an off-path sender from binding a victim's address; a token holder that can
spoof its source can still keep an existing binding alive with spoofed keepalives.

### 2.3 RTP fixed header (RFC 3550)

Every media packet starts with a standard RTP header, so Wireshark can decode the
media port with "Decode As... RTP".

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|V=2|P|X|  CC   |M|     PT      |       sequence number         |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           timestamp                           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|           synchronization source (SSRC) identifier            |
+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+
|            contributing source (CSRC) identifiers             |
|                             ....                              |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

* Senders use V=2, P=0, X=0. Receivers handle P=1 (strip padding) and X=1 (skip the
  extension) as RFC 3550 requires, and reject anything shorter than the header claims.
* CC is 0 for client streams. The server's mixed audio stream lists the SSRCs of the
  members whose audio is in the packet (at most 15).
* Each participant has two SSRCs, both assigned by the server at join time: one for audio
  and one for video. Each SSRC has its own sequence number space and clock. The server
  only accepts an SSRC from the address registered for its owner.

Payload types:

| PT  | Content | Clock rate | Marker bit |
|-----|---------|------------|------------|
| 96  | JPEG video fragment (section 2.4) | 90 000 Hz | set on the last fragment of a frame |
| 97  | L16 audio, 16 kHz mono, 20 ms per packet (section 2.5) | 16 000 Hz | set on the first packet after silence |
| 127 | XOR FEC parity (section 2.6) | same as the protected stream | 0 |

### 2.4 Video payload (PT 96)

A JPEG frame is split into fragments of at most 1200 bytes. Each fragment is one RTP
packet with this 8-byte payload header after the RTP header:

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                       frame id (uint32)                       |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|      fragment index           |       fragment count          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 JPEG bytes (1..1200) ...                      |
```

* `frame id` increases by one per frame and wraps at 2^32.
* `0 <= fragment index < fragment count <= 1024`, so a frame is at most
  1024 x 1200 = 1,228,800 bytes (1.17 MiB); larger frames are not sent.
* All fragments of a frame share the RTP timestamp (capture time, 90 kHz) and use
  consecutive sequence numbers, with FEC packets in between (section 2.6).
* Frames are independent JPEG images, so a receiver can show frame n+1 even if frame n was
  lost; there is no error propagation between frames.

### 2.5 Audio payload (PT 97)

320 samples of signed 16-bit PCM, big-endian as RFC 3551 (L16) specifies, mono at
16 kHz: 640 bytes, 20 ms. The RTP timestamp counts samples. The server drops audio packets
of any other length.

### 2.6 FEC parity (PT 127)

A simplified version of RFC 5109 (ULPFEC) parity without the protection mask: one parity
packet protects `k` consecutive packets of the same SSRC and can rebuild any single one of
them. The parity packet uses the SSRC and the sequence number space of the stream it
protects (as WebRTC does with ULPFEC), so a group of `k` media packets with sequence
numbers `b .. b+k-1` is followed by its parity packet with sequence number `b+k`.

```
RTP header: V=2, PT=127, M=0, SSRC = protected SSRC, seq = next seq of the stream,
            timestamp = timestamp of the last protected packet
FEC header (12 bytes):
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|        SN base (uint16)       |   count k     |  reserved (0) |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
| byte0 recovery| byte1 recovery|        length recovery        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      timestamp recovery                       |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|            parity payload (XOR of protected bodies) ...       |
```

For each protected packet, its *body* is everything after the 12-byte fixed header (CSRC
list, payload header, payload) and its *length* is `len(packet) - 12`.

* `count k`: 1 to 48 packets, protecting sequence numbers `SN base .. SN base + k - 1`
  (mod 2^16).
* `byte0 recovery` = XOR of `(byte0 & 0x3F)` (P, X and CC bits) of the protected packets.
* `byte1 recovery` = XOR of byte 1 (M and PT).
* `length recovery` = XOR of the lengths.
* `timestamp recovery` = XOR of the timestamps.
* `parity payload` = XOR of the bodies, each zero-padded to the longest one.

To rebuild the single missing packet, XOR the parity packet's fields with the same fields
of the `k-1` packets that arrived: the result gives byte 0 (with V=2 added back), byte 1,
the length, the timestamp and the body (cut to the recovered length). The sequence number
is the one missing from the group and the SSRC is the parity packet's SSRC.

Grouping rules used by the senders in this repository:

* Video: groups never cross a frame boundary. A frame of `n` fragments gets `ceil(n/k)`
  parity packets; the last group may be shorter than `k`. Default `k = 4` (25 % overhead).
* Audio: consecutive packets are grouped. The server protects each member's mixed stream
  with `k = 2` by default; the client's upstream audio uses `k = 2` too. Small groups keep
  the recovery delay (k x 20 ms) inside the audio jitter buffer.
* `k = 0` disables FEC.

### 2.7 Mixed audio stream (server to client, relay mode)

* One stream per receiver, SSRC = the room's `mixer_ssrc` (from `joined`), its own
  sequence numbers, timestamp = mixer tick x 320.
* Every 20 ms the server takes, for every sender, the frame due from a small per-sender
  jitter buffer (default 40 ms), sums all senders except the receiver in 32-bit integers
  and saturates the sum to 16 bits. The CSRC list names the senders in the mix.
* If no other member has audio for a tick, nothing is sent for that receiver
  (discontinuous transmission). The first packet after such a gap has M=1.

### 2.8 Media rules enforced by the server

For each RTP packet the server checks, in order, and drops with a counted reason:

1. the SSRC belongs to a current participant (`unknown_ssrc`);
2. the source address equals that participant's registered address (`addr_mismatch`);
3. the room is in relay mode, or within the relay grace period (`p2p_mode`);
4. the sender's ingress token bucket has room (`ingress_limited`, default 8 Mbit/s,
   256 KiB burst, counting 28 bytes of IP/UDP header per packet; the bucket belongs to
   the client, so leaving and rejoining a room does not refill it);
5. the payload type fits the SSRC's kind (`bad_payload`).

Then video and its parity packets are forwarded to every other registered member, and
audio and its parity packets go to the room's mixer. A packet from one room can never be
sent to a member of another room, because forwarding targets come only from the sender's
own room in the routing table.

### 2.9 Retransmission requests (RTCP generic NACK)

A receiver in a relay room that is missing video packets asks the server to send them
again with an RTCP generic NACK (RFC 4585, section 6.2.1):

```
 0                   1                   2                   3
|V=2|P| FMT=1 |   PT=205      |          length (words - 1)   |
|              SSRC of packet sender (the requester's own SSRC) |
|              SSRC of media source (the video stream)          |
|            PID                |             BLP               |  1 to 16 items
```

PID is a lost sequence number; bit i of BLP set means PID + i + 1 is lost too, so one
item covers up to 17 packets and one NACK up to 272. No padding, no compound RTCP.

The server keeps the last 256 video packets (fragments and parity packets) of every
sender (`-nack-buffer`) and answers a NACK by sending the listed packets, unchanged
(same SSRC and sequence number), to the requester only. It drops the NACK, with the
reasons of section 2.8, unless:

1. the packet-sender SSRC is one of the requester's own SSRCs and the datagram comes
   from its registered address (`unknown_ssrc`, `addr_mismatch`);
2. the room is relaying (`p2p_mode`) and the request fits the requester's ingress
   bucket (`nack_limited`);
3. the media SSRC is the video stream of another member of the same room
   (`bad_payload`).

Packets no longer in the buffer are skipped and counted (`video_rtx_misses_total`).
The retransmitted bytes count against a second token bucket of the requester
(`-max-rtx-kbps`, default 8 Mbit/s, 256 KiB burst; it stays finite when the ingress
limit is off), so a receiver cannot make the server send more than that; packets over
it are dropped as `rtx_budget`.

The client requests a missing packet 5 ms after a later one arrives (so that
reordering and FEC get the first chance), repeats the request every 30 ms, at most
three times and for at most 250 ms, and does not request jumps of more than 128
sequence numbers. In P2P mode it sends no NACKs, because peers keep no buffer. Audio
is not retransmitted; it relies on FEC (section 2.6) and the mixer's concealment.
