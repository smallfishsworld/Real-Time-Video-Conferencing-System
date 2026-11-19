# Engineering notes

This file explains each significant change from the original course project (kept
unchanged in [`legacy/`](../legacy/)) to the v2 system, and every decision that the
specification left open. Each entry gives the problem, what was measured, the fix and
its trade-offs. Numbers come from the JSON files in [`bench/results/`](../bench/results/);
[`BENCHMARKS.md`](BENCHMARKS.md) has the methodology and full tables.

## 1. What was wrong with the original design

Each item was checked against the code in `legacy/` before it was fixed.

| # | Problem in `legacy/` | Where | Consequence |
|---|---|---|---|
| 1 | Media datagrams are `pickle.dumps((ID, kind, data))` and the receiver calls `pickle.loads()` on whatever arrives on its UDP port | `my_client.py` `keep_share` / `keep_recv` | Anyone who can send one UDP packet to a client can run code on it |
| 2 | A whole JPEG frame (about 31 kB for the test corpus) is one UDP datagram | `keep_share` | On a 1500-byte MTU it becomes 21 or 22 IP fragments; losing any one loses the frame (section 3.1) |
| 3 | No sequence numbers, jitter buffer, FEC or retransmission | media threads | No way to detect, conceal or repair loss |
| 4 | No audio mixing: each received chunk is written to a blocking output stream, one after another | `keep_recv` | With two or more talkers playback falls behind real time without bound, and because the same thread receives video, video stalls too (section 3.2) |
| 5 | The relay identifies senders by IP address and skips every receiver with the sender's IP | `my_server.py` `handle_data` | Two clients on one host cannot hear or see each other |
| 6 | Ports are derived from user IDs (TCP 20000+ID, UDP 20000+100·ID, conference ports 10000+ID) | `my_client.py`, `my_server.py` | At most about 455 users per host; fixed, guessable ports; one UDP socket and thread per meeting |
| 7 | TCP messages assume that one `recv()` returns one message (the MainServer reads each 10-byte field with its own `recv(10)`) | `my_server.py` | Requests coalesced by TCP are mis-parsed; the benchmark harness has to pause between fields to work around it |
| 8 | Busy-wait loops (`while True: ... time.sleep(0.0001)` / `0.02`) | client threads | CPU burned while idle; latency quantised to the sleep |
| 9 | Camera, microphone and `pyautogui` are opened at import time; tkinter popups | `util.py`, client | The client cannot start without devices; no headless mode |
| 10 | Hard-coded LAN IP in `config.py` | `config.py` | Must edit code to run anywhere else |

## 2. Changes, with the problem each one solves

### 2.1 An explicit wire protocol instead of pickle (fixes 1, 3, 6, 7)

**Fix.** [`PROTOCOL.md`](PROTOCOL.md) defines two planes. Control is length-prefixed
JSON over one TCP port (4-byte big-endian length, at most 64 KiB, `seq` echoed in
replies). Media is RTP (RFC 3550) on one UDP port for the whole server, with a small
fragmentation header for video, L16 audio and an XOR parity format modelled on RFC 5109.
The Go decoders are native-fuzzed (`server/internal/protocol/fuzz_test.go`, seed corpus
committed); both implementations are tested against the same golden vectors in
`protocol-testdata/vectors.json`. Readers read the 4-byte length and then exactly that
many bytes, so TCP segmentation does not matter: `client/tests/test_protocol.py`
feeds the client's reader (`read_frame`) every vector one byte per read.

**Trade-off.** More code than `pickle`, and a version field to maintain. In exchange the
packets are inspectable (Wireshark "Decode As... RTP" works) and nothing executes data
from the network.

### 2.2 MTU-sized fragments and XOR FEC (fixes 2 and 3)

**Problem, measured.** B1 sends the same 480x480 JPEG corpus (mean 31 090 bytes) through
a 1500-byte-MTU veth link with random UDP drops. At 1 % loss the original transport
delivered 65.5 % of frames within 200 ms; at 5 % loss, 11.9 %.

**Fix.** Senders split each frame into fragments of at most 1200 bytes (1232-byte UDP
payload at most, so no IP fragmentation on any 1500-byte path, IPv4 or IPv6) and add one
XOR parity packet per group of k = 4 fragments; groups never span frames, so a frame can
be rebuilt as soon as its own packets are in. Audio uses k = 2 in both directions.

**Measured.** Frames delivered within 200 ms (B1):

| Loss | legacy | v2 without FEC | v2 with FEC |
|---|---|---|---|
| 1 % | 65.5 % | 78.0 % | 99.5 % |
| 2 % | 36.0 % | 58.8 % | 97.3 % |
| 5 % | 11.9 % | 29.7 % | 88.7 % |
| 10 % | 3.6 % | 5.5 % | 57.8 % |

Fragmentation alone helps less than one might expect, because a frame of about 26
fragments is lost if any of them is lost, just as a datagram of 21-22 IP fragments is; the measured v2-without-FEC
numbers match the independent-loss model (78.0 % against 76.6 % at 1 %). What makes the
difference is that application-level fragments can be protected and repaired, which IP
fragments cannot.

**Trade-off.** FEC costs 25 % more video bandwidth (one parity per four fragments) and
50 % more audio bandwidth; together with per-fragment RTP headers, v2 video uses about
32 % more downstream bandwidth than the original in B2 (20 587-20 590 kbit/s against
15 564-15 569 kbit/s for four senders). XOR parity repairs only one loss per group; at
10 % loss groups with two losses are common, which is why delivery falls to 58 %.
Retransmission (section 2.11) repairs the rest; a stronger code (Reed-Solomon) would be
the alternative where round trips are long.

### 2.3 A surprise: the original is worse than the independent-loss model

At 1 % loss the independent-loss model predicts 80.5 % delivery for 22-fragment
datagrams, but the original delivered 65.5 %, and 80.7 % in the first half of the run
against 50.3 % in the second half. The receiver's kernel counted 2 444 IP reassembly
failures. The explanation that fits these counters: every incomplete datagram leaves
its fragments in the reassembly queue until `ipfrag_time` (30 s) expires; at 1 % loss
about one frame in five is incomplete, so the queue memory grows during the run until
it reaches `ipfrag_high_thresh` (4 MiB), after which the kernel drops new fragments,
including those of datagrams that would have been complete. Audio suffers too: 20.0 % of
the original's 2-fragment audio datagrams were lost at 1 % packet loss, where independent
loss would lose 1 - 0.99^2 = 2 %. v2 never sends fragmented datagrams, so its reassembly counters stay at zero.
`bench/run_b1.sh` samples the receiver namespace's reassembly memory
(`/proc/net/sockstat`, `FRAG:` line): it peaked at 4.00 MiB, the threshold, in every
lossy run of the original and stayed at zero for v2 (`receiver_frag_memory` in
`bench/results/b1_loss.json`).

### 2.4 Server-side mix-minus audio (fixes 4)

**Problem, measured.** B2 runs 3- and 5-party calls in which everybody talks and sends
video. The original client's receive thread writes each received audio chunk to a
blocking output stream. In the benchmark that stream is a fake PyAudio device
(`bench/legacy_harness/stubs.py`) whose `write()` blocks until the previous chunk has
played, which is how a blocking sound device behaves. With two remote talkers the
receiver must play 2 x real time of audio, so its UDP socket buffer overflows and the
latency settles at the buffer depth: median capture-to-playout latency was 594 ms
(589-614 over three runs) with three parties and 978 ms (952-1024) with five, where it
was still rising at the end of the 62 s window. Only 41-59 % (three parties) and
15-38 % (five parties) of each talker's chunks were played, and because the same thread
receives video, the receivers processed 25.0 % (three parties) and 8.2 % (five parties,
median of three runs) of the video frames; with five parties, in 14 of 15 receiver
runs one sender's video never arrived during the whole window. Both ends are
simulated (synthetic microphones, stand-in speakers, loopback), and the original's
numbers depend on its stand-in speaker (`docs/BENCHMARKS.md`, B2).

**Fix.** The server runs one 20 ms mixer per room. Each sender's audio goes through a
small jitter buffer (2 frames); every tick the mixer sums all active senders in int32
once and, for each receiver, subtracts the receiver's own frame and saturates to int16
(mix-minus in O(senders + receivers) instead of O(senders x receivers)). Each receiver
gets one stream with a CSRC list naming the talkers in it. The client plays it through a
60 ms jitter buffer with packet-loss concealment (repeat the last frame at half
amplitude, at most three times).

**Measured.** v2 median capture-to-playout latency: 119 ms with three parties (117-119
over three runs) and with five (119-124), with every talker's audio played and every
video frame processed (`bench/results/b2_audio.json`). Downstream audio per receiver is
constant in the number of talkers: 415 and 420 kbit/s including audio FEC (275 and
278 kbit/s without it), against 525 and 1 050 kbit/s for the original, which forwards
every talker's stream separately.

**Trade-off.** The server now decodes and re-encodes audio (cheap for L16: a median of
7.4 µs per 4-member tick in the microbenchmark, `bench/results/micro.json`) and adds a
mixer jitter buffer of 40 ms. A receiver
cannot mute individual talkers locally any more.

### 2.5 Identity by SSRC and token, not by IP address (fixes 5 and 6)

**Fix.** At join time the server assigns each participant an audio SSRC, a video SSRC
and a random 128-bit token. The client sends `REGISTER` (with the token) from its media
socket until it is acknowledged, like a STUN binding request; the server then accepts an
SSRC only from that registered address, and forwards only to members of the sender's own
room (the routing table is built per room). Several clients can run on one host (the
end-to-end test runs three on 127.0.0.1), spoofed packets with another participant's
SSRC are dropped (`addr_mismatch`), and a packet can never reach another room. All media
uses one UDP port, so there is no per-meeting socket, thread or port range.

### 2.6 Mode decision on the server, with grace periods

The course specification asks for P2P with two participants and automatic switching. The
server owns the decision: P2P when P2P is enabled, the room has exactly two members and
both have registered; relay otherwise. It sends `mode` with the peer's observed address.
After a relay-to-P2P switch the server keeps relaying for 1 s, and clients accept media
from both paths for 2 s after any switch, so in-flight packets on the old path are not
lost. The integration and end-to-end tests check that the server forwards nothing in P2P
mode (its counters stay flat) and that P2P -> relay -> P2P works.

### 2.7 Concurrency without per-packet locks or allocations (server hot path)

- The room registry holds the only lock; after every change it publishes an immutable
  view. The media router turns it into a routing table (SSRC -> sender, room, forwarding
  targets) and swaps it in with an `atomic.Pointer`, so the per-packet path takes no
  lock except the sender's own token bucket.
- Each room's mixer runs in its own goroutine and reads its member list from an
  atomically swapped snapshot.
- Buffers are fixed per goroutine; the microbenchmarks report 0 allocations per packet
  forwarded and per mixer tick (`bench/results/micro.json`).
- Each control connection has one reader and one writer goroutine with a bounded outbox;
  a client that does not drain its outbox is disconnected instead of blocking the
  registry. Replies are queued before the events they cause, so a client always sees
  `joined` before the first `roster`.

### 2.8 Client rewrite (fixes 8, 9, 10)

The client is an asyncio program: the control connection, the media socket, the
registration loop and the video sender are tasks; the audio device runs on its own thread
and is the playout clock. Devices are opened only when a meeting starts, through thin
adapters (OpenCV camera and window, PyAudio microphone and speaker, all optional imports)
with synthetic replacements (`--fake-media`: a moving test pattern and a tone). Server
address and port are command-line flags.

### 2.9 The price of MTU-safe packets: server CPU (B3)

**Measured.** In B3 (`bench/results/b3_capacity.json`: loopback with Linux's default
64 KiB MTU, rooms of four at 15 fps, three repeats), the original relay passed at 448
participants, the most its port scheme allows, with its process at 0.39 of a CPU. v2
with one system call per datagram (`-udp-batch=1`, as before section 2.10) passed up to
160, 176 and 192 participants in the three repeats, with its server at 0.78 of the CPU
it was pinned to at 160 participants and 0.96 at 208. At 16 participants it used 0.597
CPU-seconds per participant-minute against 0.158 for the original.

**Why.** v2 sends 18.8 times as many packets for the same media (609 296 against
32 340 in each fixed-load run): about 26 fragments and 7 parity packets per frame
and 75 audio packets per second, against one datagram per frame or audio chunk, and on
a 64 KiB-MTU loopback the original's 31 kB datagram is never fragmented. With
`-udp-batch=1` the server pays one `recvfrom` per incoming packet and one `sendto` per
copy.

**Trade-off.** MTU-safe fragments are what make FEC and retransmission (section 2.11)
possible and keep the original's IP-fragmentation failure modes (section 2.3) away, so
they stay. Section 2.10 is what profiling then recovered of the CPU price.

### 2.10 Profile first: the cost was packets, not system calls (B3)

**Measured.** A CPU profile of the server under the B3 load at 128 participants
(`bench/profile_b3.sh`, server pinned to one CPU, `bench/results/pprof_v2-*.txt`, built
at 4fe5eca) put 84.6 % of its samples under `Syscall6`, 75.9 % in forwarding (one
`sendto` per copy) and 13.4 % in reading (one `recvfrom` per datagram). On one binary,
switching the mechanisms on in turn (one 15 s profile each):

| Server flags | Server CPU (of one core) | Datagrams per read call | Copies per send call | Datagrams per message |
|---|---|---|---|---|
| `-udp-batch=1` (one call per datagram) | 0.698 | 1.0 | 1.0 | 1.0 |
| `-udp-gso=false` (`recvmmsg` + `sendmmsg`) | 0.645 | 23.0 | 95.0 | 1.0 |
| default (`recvmmsg` + `sendmmsg` + UDP GSO) | 0.522 | 18.9 | 80.6 | 10.1 |

Making 18.6 times fewer system calls saved 7.6 % of the CPU; sending 7.05 times fewer
messages down the UDP/IP send path saved a further 19.0 %, 25.1 % in all. The B3 sweeps
(`bench/results/b3_capacity.json`, three repeats per size) agree:

| Server flags | Largest size passed, per repeat | Server CPU at 128 participants | CPU-seconds per participant-minute (16 participants) |
|---|---|---|---|
| `-udp-batch=1` | 160, 176, 192 | 0.680 (0.664-0.685) | 0.597 (0.572-0.613) |
| `-udp-gso=false` | 192, 192, 192 | 0.628 (0.607-0.657) | 0.565 (0.552-0.570) |
| default | 304, 304, 272 | 0.508 (0.496-0.535) | 0.503 (0.479-0.503) |

(medians of three runs, range in brackets). Every repeat passed up to 160, 192 and 272
participants respectively; at 128 participants the CPU fell by 7.6 % with batching and
by a further 19.1 % with GSO (25.3 % in all), and at 16 participants, where reads are
small, by 5.3 % and 11.1 % (15.8 %). These are loopback numbers: the namespace's lo
splits every GSO message into datagrams before the receive path (`gso_max_segs 1`), and
the receivers' receive path is charged to the server's CPU for every variant
(`docs/BENCHMARKS.md`, B3).

**Why.** The time under `Syscall6` is the kernel's network stack, not the price of
entering it: every datagram still gets a route lookup, an skb, the netfilter hooks and,
on loopback, the whole receive path. UDP GSO (`UDP_SEGMENT`) hands the kernel one
message of up to 64 equal-length datagrams; the stack runs once and the message is
split as late as possible (by the NIC, with USO, or just before the driver).

**How.** `server/internal/udpio` wraps `recvmmsg`/`sendmmsg` over `x/sys/unix` with
reused header arrays and a `RawConn` callback built once, so neither path allocates
(tested with `testing.AllocsPerRun`). `x/net/ipv4`'s `ReadBatch`/`WriteBatch`, the
obvious choice, allocates a `*net.UDPAddr` per received datagram and has no
per-message control data for `UDP_SEGMENT`. Within each read the router groups every
sender's fragments, then its parity packets, so that each list is a run of
equal-length datagrams (fragments are 1220 bytes, parity packets 1232; a shorter last
fragment ends a run). Fallbacks: one call per datagram on macOS, Windows and 32-bit
platforms or with `-udp-batch 1`; if the kernel refuses a GSO message (a device without
checksum offload, Linux before 4.18), its datagrams are sent again one by one and GSO
is switched off.

**Costs and limits.** A read's parity packets can now arrive after that read's
fragments; receivers never depended on the order. GSO only merges what one read
returned, so it helps less at light load (small reads) than near saturation. On
loopback the receive work of a datagram runs inside whichever system call on that CPU
triggers the softirq, so the profile's split between forwarding, reading and mixing is
approximate (the mixers' share was 5.4, 10.1 and 8.2 % in the three profiles); the
whole-process CPU time is the reliable number. The mixer still sends one `sendto` per
receiver and tick.

### 2.11 NACK retransmission for video (B1)

**Problem.** XOR parity repairs one loss per group of four; at 5 % and 10 % loss a
26-fragment frame often loses two packets of a group, and the frame is gone.

**Change.** The server keeps every sender's last 256 video packets in a ring indexed by
sequence number; a receiver that sees a gap sends an RTCP generic NACK (RFC 4585) and
gets the listed packets again, to itself only. Requests come from the registered
address, must name another member's stream in the same room, and the retransmitted
bytes are charged to a per-requester token bucket, so NACK cannot be used to make the
server amplify traffic. The client waits 5 ms before asking (so FEC and reordering get
the first chance), retries every 30 ms, at most three times and for 250 ms.

**Measured.** In B1 (`bench/results/b1_loss.json`, all five systems built from one
commit) FEC plus NACK delivered 100.0 % of frames complete within 200 ms at 1, 2 and 5 %
random loss and 99.7 % at 10 %, against 99.5, 97.3, 88.7 and 57.8 % with FEC alone. At
10 % loss the receiver sent 485 NACKs for 1 061 packets (counting repeats), 1 004 of them
then arrived or were rebuilt from parity, and one was given up. Repaired frames arrive
later: at 10 % loss the p99 frame latency rose from 2.6 to 17.0 ms, the median stayed at
1.3 ms. The testbed's round trip is under a millisecond, so this is mostly the client's
5 ms wait and 10 ms request interval; on a real path every repaired frame arrives one
round trip later still.

**A finding: NACK alone misses lost tails.** Without FEC, NACK delivered only
98.5-99.1 % at every loss rate, although its model (four tries per packet) predicts
99.7 % or more. A receiver notices a loss when a later packet arrives; for the last
fragments of a frame that is the next frame, one frame interval (64 ms) later. The
receiver's frame assembler is newest-wins (JPEG frames are independent, so a frame that
completes after a newer one is useless), and when the next frame arrives complete before
the retransmission, the older frame is dropped. All 31 frames that NACK alone lost were
followed by a complete frame, 28 of them complete within 0.6 ms of their first fragment
(`bench/results/b1_lost_frames.txt`), and the NACK-only p99 latency at 5 and 10 % loss,
79.5-79.7 ms, is that one-frame wait plus the request delay. With FEC the parity packet
that closes every frame reveals a lost tail at once and usually repairs it, which is one
more reason the client uses both by default. Every fragment carries its frame's fragment
count, so a receiver could also request a missing tail after a short timeout instead of
waiting for the next frame; that is not implemented.

**Costs.** One 1.2 kB copy per received video packet into the ring (146 ns per packet in
`BenchmarkForwardVideo`: 1 155 ns with the buffer against 1 009 ns without, for a packet
forwarded to three receivers in batches of 64 with GSO, `bench/results/micro.json`), up
to 315 kB of memory per sender once it sends video, and upstream NACK traffic. Audio is
not retransmitted: the mixer's output is one stream per receiver, and FEC plus
concealment is cheaper at 20 ms frames. Peers in P2P mode keep no buffer, so a P2P room
has FEC only.

## 3. Decisions the specification left open

| Topic | Decision | Reason |
|---|---|---|
| Audio format | L16, 16 kHz mono, 20 ms frames, big-endian (RFC 3551) | 16 kHz is what the original used; 20 ms is the usual RTP audio frame; big-endian is what L16 specifies |
| Video | JPEG per frame, quality 85, 480x480 in benchmarks (the original's settings) | Frames are independent, so loss never propagates; no codec dependency |
| Cancel rights | Only the creator (host) can cancel; the host role is not transferred when the host leaves | The specification gives the cancel right to the creator |
| Room size | 16 members by default (`-max-room-size`), 256 rooms, 1024 clients | Conservative limits; configurable |
| Chat timestamps | Stamped by the server (name and server time) | Clients' clocks are not trusted |
| Rate limits | Server: 8 Mbit/s ingress per sender (token bucket, 256 KiB burst), 50 control requests/s per connection, 2000 REGISTERs/s in total. Client: 6000 kbit/s send cap; whole video frames are dropped rather than single fragments when over budget | Owner requirement (campus network); dropping whole frames avoids sending fragments of frames that cannot be completed |
| Empty room | Closed when the last member leaves; ids are never reused | Specification: a meeting ends when everyone has left |
| Observability | Prometheus `/metrics` and optional pprof on 127.0.0.1:6101 | Never exposed on a public interface by default |
| Batched I/O | `recvmmsg` up to 64 datagrams, `sendmmsg` per read, UDP GSO for each sender's runs (`-udp-batch`, `-udp-gso`) | Profile-driven (section 2.10); the flags keep the old one-call-per-datagram path for comparison |
| Retransmission | Generic NACK for video only, 256 packets per sender, relay mode only (`-nack-buffer`) | Standard RTCP feedback; audio relies on FEC; peers have no buffer |

## 4. Benchmark methodology decisions

These choices are about fairness between the original and v2; each is also described
where it is used.

- **The original runs unmodified.** `bench/legacy_harness/` imports the real
  `legacy/my_server.py` and `legacy/my_client.py` after installing stub modules for
  `pyaudio`, `cv2`, `pyautogui`, `PIL.ImageGrab` and `tkinter`, and overrides
  `config.SERVER_IP`. Accommodations, all needed only to make the original run in a
  harness: the camera function is replaced by one that returns corpus frames (so both
  systems send identical JPEG bytes); each participant binds its own IP address (the
  relay skips same-IP receivers, problem 5); the client object is built without its
  interactive UI and set to relay mode (`p2p = False`), as the original's `chgmode`
  would; the harness pauses 50 ms between the MainServer's 10-byte fields (problem 7).
  The client's videoplayer, auto_mode_switch and debug threads are not started (none
  touches the sockets). auto_mode_switch would choose P2P for a two-member call, so the
  original would run B1's two-member call P2P; B1 forces relay because the brief compares
  the relayed path and its topology has no sender-to-receiver route. Both systems then
  cross the same lossy server -> receiver link, so the loss comparison is unaffected.
- **Same media load.** Both systems send the same corpus frames. B1 and B2 send one frame
  per 1024-sample audio chunk (15.625 fps), the original client's own rate, because the
  original's loop blocks on the microphone read; B3 uses 15 fps for both.
- **P2P disabled in B1** on the v2 server (`-p2p=false`) so that both systems relay
  through the server, as the original does in relay mode.
- **Two loss conditions in B1.** In the main one, nftables drops packets in the
  receiver's prerouting chain at priority -500, before defragmentation (-400), so single
  IP fragments are lost as on a real network; only the server -> receiver direction
  loses packets and the round trip is under a millisecond, the best case for NACK. In
  the harder one (v2 only), a userspace relay drops datagrams independently in both
  directions and adds 25 ms each way, so NACKs and retransmissions can be lost and every
  repair costs at least 50 ms. TCP control traffic is never impaired.
- **Delivery definitions.** B1 counts a frame as delivered when the complete frame was
  at the receiver within 200 ms of being sent. In B3 the load generator does not use FEC
  to repair frames, so v2's delivery there is a lower bound.
- **CPU accounting.** B3 reads utime + stime of the server process from
  `/proc/<pid>/stat` at the edges of the measurement window, with the server pinned to
  one CPU and the load generator to two others (`taskset`). On loopback the receive path
  of a packet runs in softirq on the sending CPU, and with tick-based accounting
  (`CONFIG_IRQ_TIME_ACCOUNTING=n`, recorded in each result) that time is charged to the
  sending process: the server pays for its receivers' IP receive path, and the load
  generator for the server's. Each B3 point records how much of the server core's busy
  time was softirq. To keep that receive path the same for every variant, lo's
  `gso_max_segs` is 1, so GSO messages are split before lo as in front of a NIC without
  segmentation offload; with lo's default they would go up the receivers' stack whole,
  which no real network does and which made GSO look better in the first B3 runs.
- **Clocks.** All processes of a run are on one host and use CLOCK_MONOTONIC, so one-way
  latencies need no clock synchronisation.
- **GSO in B1.** veth passes UDP GSO messages whole into the receiver's namespace, where
  the loss rule would drop a whole message at once. `gso_max_segs 1` on the server's
  veth makes the kernel split them as it transmits, so the rule sees single datagrams,
  as a lossy wire does.
- **Before/after on one binary.** The batching comparison in B3 and in the profiles
  uses one build with `-udp-batch=1` (one system call per datagram, the old pattern) and
  `-udp-gso=false`, so nothing else differs.
- **Repeats, interleaved.** Every B1, B2 and B3 point runs three times, interleaved by
  repeat, so slow drifts in the shared machine's load hit every system alike. Results
  give the median and the range of the repeats; B1 also pools its frames into a Wilson
  95 % interval, which assumes independent frames and is too narrow where losses come
  in bursts.
- **NACK in the main B1 condition sees a short round trip.** NACKs always arrive and a
  retransmission comes back within the client's 5 ms wait plus its 10 ms request
  interval. The harder condition shows NACK on a 50 ms round trip with loss both ways.
- **Provenance.** Each invocation has its own run ID, build directory, network
  namespaces and raw directory, and its result is made from that directory only. Results
  record the commit and any uncommitted files both when the binaries were built
  (`environment.build`, with the binaries' SHA-256) and when the results were written
  (`environment.git_dirty_files`); files under `bench/results` do not count.
