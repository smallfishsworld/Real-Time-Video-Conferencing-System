package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// joinedCapture is a room.Notifier that remembers the last `joined` message.
type joinedCapture struct {
	mu     sync.Mutex
	joined protocol.Joined
}

func (c *joinedCapture) Send(frame []byte) {
	body, err := protocol.ReadFrame(bytes.NewReader(frame), nil)
	if err != nil {
		panic(err)
	}
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Type == protocol.TypeJoined {
		c.mu.Lock()
		_ = json.Unmarshal(body, &c.joined)
		c.mu.Unlock()
	}
}

type testServer struct {
	t      *testing.T
	reg    *room.Registry
	router *Router
	addr   netip.AddrPort
}

func newTestServer(t *testing.T, roomCfg func(*room.Config), mediaCfg func(*Config)) *testServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	// The buffer request app.Start makes with its default SocketBuffer.
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(4 << 20)
	rc := room.DefaultConfig()
	rc.RelayGrace = 0
	if roomCfg != nil {
		roomCfg(&rc)
	}
	reg := room.NewRegistry(rc)
	mc := DefaultConfig()
	if mediaCfg != nil {
		mediaCfg(&mc)
	}
	router := NewRouter(conn, reg, mc, Hooks{})
	reg.Observe(router.OnView)
	go func() { _ = router.Serve() }()
	t.Cleanup(func() { _ = router.Close() })
	return &testServer{t: t, reg: reg, router: router, addr: router.LocalAddr()}
}

// peer is a fake client: a control-plane membership plus a UDP socket.
type peer struct {
	t      *testing.T
	client *room.Client
	joined protocol.Joined
	conn   *net.UDPConn
	seq    uint16
}

func (s *testServer) peer(name string, roomID uint32) *peer {
	s.t.Helper()
	capt := &joinedCapture{}
	c, err := s.reg.Hello(name, capt)
	if err != nil {
		s.t.Fatal(err)
	}
	if roomID == 0 {
		err = s.reg.CreateRoom(c, "", nil)
	} else {
		err = s.reg.JoinRoom(c, roomID, nil)
	}
	if err != nil {
		s.t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	// A burst can wait here until recv reads it (the load generator asks for the same).
	_ = conn.SetReadBuffer(4 << 20)
	capt.mu.Lock()
	p := &peer{t: s.t, client: c, joined: capt.joined, conn: conn}
	capt.mu.Unlock()
	// The router learns the new token from its next routing table.
	s.router.Sync()
	return p
}

// register performs the REGISTER exchange: a challenge, the echo of its nonce and the
// final REGISTER_ACK.
func (p *peer) register(server netip.AddrPort) {
	p.t.Helper()
	ack, challenges, err := registerConn(p.conn, server, p.joined.Token, 99)
	if err != nil {
		p.t.Fatal(err)
	}
	if challenges != 1 {
		p.t.Fatalf("%d challenges before the binding, want 1", challenges)
	}
	if ack.Observed != p.conn.LocalAddr().(*net.UDPAddr).AddrPort() {
		p.t.Fatalf("observed %v, local %v", ack.Observed, p.conn.LocalAddr())
	}
}

// registerConn sends REGISTER for token from conn, answers challenges by echoing their
// nonce, and returns the final REGISTER_ACK and the number of challenges.
func registerConn(conn *net.UDPConn, server netip.AddrPort, token string, txID uint64) (protocol.RegisterAck, int, error) {
	tok, err := protocol.ParseToken(token)
	if err != nil {
		return protocol.RegisterAck{}, 0, err
	}
	reg := protocol.Register{TxID: txID, Token: tok}
	buf := make([]byte, protocol.RegisterSize)
	ackBuf := make([]byte, 64)
	challenges := 0
	for challenges < 4 {
		n, _ := reg.MarshalTo(buf)
		if _, err := conn.WriteToUDPAddrPort(buf[:n], server); err != nil {
			return protocol.RegisterAck{}, challenges, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var ack protocol.RegisterAck
		for {
			m, err := conn.Read(ackBuf)
			if err != nil {
				return protocol.RegisterAck{}, challenges, fmt.Errorf("no REGISTER_ACK: %w", err)
			}
			if protocol.ParseRegisterAck(ackBuf[:m], &ack) == nil && ack.TxID == txID {
				break
			}
		}
		_ = conn.SetReadDeadline(time.Time{})
		if !ack.Challenge {
			return ack, challenges, nil
		}
		challenges++
		reg.HasNonce, reg.Nonce = true, ack.Nonce
	}
	return protocol.RegisterAck{}, challenges, fmt.Errorf("still challenged after %d echoes", challenges)
}

func (p *peer) videoPacket(frameID uint32) []byte {
	h := protocol.RTPHeader{PayloadType: protocol.PTVideo, SequenceNumber: p.seq, Timestamp: frameID * 6000, SSRC: p.joined.VideoSSRC, Marker: true}
	p.seq++
	buf := make([]byte, 64)
	n, _ := h.MarshalTo(buf)
	v := protocol.VideoHeader{FrameID: frameID, FragmentIndex: 0, FragmentCount: 1}
	m, _ := v.MarshalTo(buf[n:])
	n += m
	n += copy(buf[n:], "jpeg")
	return buf[:n]
}

func (p *peer) send(server netip.AddrPort, pkt []byte) {
	p.t.Helper()
	if _, err := p.conn.WriteToUDPAddrPort(pkt, server); err != nil {
		p.t.Fatal(err)
	}
}

// recv returns the datagrams that arrive within d.
func (p *peer) recv(d time.Duration) [][]byte {
	var out [][]byte
	deadline := time.Now().Add(d)
	for {
		_ = p.conn.SetReadDeadline(deadline)
		buf := make([]byte, 2048)
		n, err := p.conn.Read(buf)
		if err != nil {
			return out
		}
		out = append(out, buf[:n])
	}
}

// collect reads p's datagrams in the background, so that a burst does not have to fit
// in the socket buffer, until none has arrived for quiet. The returned function waits
// for that and returns them.
func (p *peer) collect(quiet time.Duration) func() [][]byte {
	done := make(chan [][]byte, 1)
	go func() {
		var out [][]byte
		for {
			_ = p.conn.SetReadDeadline(time.Now().Add(quiet))
			buf := make([]byte, 2048)
			n, err := p.conn.Read(buf)
			if err != nil {
				done <- out
				return
			}
			out = append(out, buf[:n])
		}
	}()
	return func() [][]byte { return <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// batchModes runs a router test with recvmmsg/sendmmsg batching and UDP GSO (the
// defaults), with batching alone, and with one system call per datagram.
func batchModes(t *testing.T, f func(t *testing.T, mediaCfg func(*Config))) {
	for _, m := range []struct {
		batch int
		gso   bool
	}{{DefaultConfig().UDPBatch, true}, {DefaultConfig().UDPBatch, false}, {1, false}} {
		t.Run(fmt.Sprintf("udp-batch=%d,gso=%v", m.batch, m.gso), func(t *testing.T) {
			f(t, func(c *Config) { c.UDPBatch, c.UDPGSO = m.batch, m.gso })
		})
	}
}

func TestRouterForwardsVideoOnlyWithinRoom(t *testing.T) {
	batchModes(t, testRouterForwardsVideoOnlyWithinRoom)
}

func testRouterForwardsVideoOnlyWithinRoom(t *testing.T, mediaCfg func(*Config)) {
	s := newTestServer(t, nil, mediaCfg)
	a1 := s.peer("a1", 0)
	roomA := a1.client.InRoom()
	a2, a3 := s.peer("a2", roomA), s.peer("a3", roomA)
	b1 := s.peer("b1", 0)
	b2, b3 := s.peer("b2", b1.client.InRoom()), s.peer("b3", b1.client.InRoom())
	for _, p := range []*peer{a1, a2, a3, b1, b2, b3} {
		p.register(s.addr)
	}

	pkt := a1.videoPacket(1)
	a1.send(s.addr, pkt)
	for _, p := range []*peer{a2, a3} {
		got := p.recv(300 * time.Millisecond)
		if len(got) != 1 || !bytes.Equal(got[0], pkt) {
			t.Fatalf("%s got %d packets", p.client.Name, len(got))
		}
	}
	for _, p := range []*peer{a1, b1, b2, b3} {
		if got := p.recv(100 * time.Millisecond); len(got) != 0 {
			t.Fatalf("%s received %d packets from room A", p.client.Name, len(got))
		}
	}
	st := s.router.Stats().Snapshot()
	if st.PacketsIn["video"] != 1 || st.PacketsOut["video"] != 2 {
		t.Fatalf("stats %+v", st)
	}
}

// A burst of fragments with a parity packet after every four reaches the server's
// socket before it reads, so a batched read returns many at once. Every copy must
// arrive; fragments keep their order, and so do parity packets (which may move behind
// the fragments of the same read).
func TestRouterForwardsBurstsInOrder(t *testing.T) {
	batchModes(t, func(t *testing.T, mediaCfg func(*Config)) {
		s := newTestServer(t, nil, mediaCfg)
		a1 := s.peer("a1", 0)
		a2, a3 := s.peer("a2", a1.client.InRoom()), s.peer("a3", a1.client.InRoom())
		for _, p := range []*peer{a1, a2, a3} {
			p.register(s.addr)
		}
		// 250 small datagrams nearly fill a default 208 KiB receive buffer (Linux charges
		// each one 832 bytes or more), so read them while they arrive.
		received := map[*peer]func() [][]byte{a2: a2.collect(time.Second), a3: a3.collect(time.Second)}
		const frags = 200
		var sentFrags, sentParity [][]byte
		var enc protocol.FECEncoder
		for i := range frags {
			pkt := a1.videoPacket(uint32(i + 1))
			sentFrags = append(sentFrags, pkt)
			a1.send(s.addr, pkt)
			if err := enc.Add(pkt); err != nil {
				t.Fatal(err)
			}
			if enc.Len() == 4 {
				buf := make([]byte, protocol.MaxSendDatagram)
				n, err := enc.Flush(buf, a1.joined.VideoSSRC, a1.seq)
				if err != nil {
					t.Fatal(err)
				}
				a1.seq++
				sentParity = append(sentParity, buf[:n])
				a1.send(s.addr, buf[:n])
			}
		}
		total := len(sentFrags) + len(sentParity)
		for _, p := range []*peer{a2, a3} {
			got := received[p]()
			if len(got) != total {
				t.Fatalf("%s got %d of %d packets", p.client.Name, len(got), total)
			}
			var gotFrags, gotParity [][]byte
			for _, pkt := range got {
				if pkt[1]&0x7f == protocol.PTFEC {
					gotParity = append(gotParity, pkt)
				} else {
					gotFrags = append(gotFrags, pkt)
				}
			}
			for _, c := range []struct {
				what      string
				got, sent [][]byte
			}{{"fragment", gotFrags, sentFrags}, {"parity packet", gotParity, sentParity}} {
				if len(c.got) != len(c.sent) {
					t.Fatalf("%s got %d of %d %ss", p.client.Name, len(c.got), len(c.sent), c.what)
				}
				for i := range c.got {
					if !bytes.Equal(c.got[i], c.sent[i]) {
						t.Fatalf("%s: %s %d out of order", p.client.Name, c.what, i)
					}
				}
			}
		}
		st := s.router.Stats().Snapshot()
		if st.PacketsIn["video"]+st.PacketsIn["video_fec"] != uint64(total) ||
			st.PacketsOut["video"] != uint64(2*total) || st.Drops["write_error"] != 0 {
			t.Fatalf("stats %+v", st)
		}
	})
}

func nackPacket(t *testing.T, sender, media uint32, lost ...uint16) []byte {
	t.Helper()
	n := protocol.NACK{SenderSSRC: sender, MediaSSRC: media}
	n.SetLost(lost)
	buf := make([]byte, 128)
	size, err := n.MarshalTo(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:size]
}

// A receiver's NACK brings back exactly the packets it names, to it alone.
func TestRouterRetransmitsOnNACK(t *testing.T) {
	batchModes(t, func(t *testing.T, mediaCfg func(*Config)) {
		s := newTestServer(t, nil, mediaCfg)
		a1 := s.peer("a1", 0)
		a2, a3 := s.peer("a2", a1.client.InRoom()), s.peer("a3", a1.client.InRoom())
		for _, p := range []*peer{a1, a2, a3} {
			p.register(s.addr)
		}
		var sent [][]byte
		for i := range 10 {
			sent = append(sent, a1.videoPacket(uint32(i+1)))
			a1.send(s.addr, sent[i])
		}
		for _, p := range []*peer{a2, a3} {
			if got := p.recv(300 * time.Millisecond); len(got) != len(sent) {
				t.Fatalf("%s got %d packets", p.client.Name, len(got))
			}
		}
		seqOf := func(pkt []byte) uint16 { return uint16(pkt[2])<<8 | uint16(pkt[3]) }
		want := [][]byte{sent[2], sent[3], sent[7]}
		a2.send(s.addr, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC,
			seqOf(sent[2]), seqOf(sent[3]), seqOf(sent[7]), seqOf(sent[9])+5))
		got := a2.recv(300 * time.Millisecond)
		if len(got) != len(want) {
			t.Fatalf("a2 got %d retransmissions, want %d", len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("retransmission %d differs from the original", i)
			}
		}
		if extra := a3.recv(100 * time.Millisecond); len(extra) != 0 {
			t.Fatalf("a3 received %d retransmissions meant for a2", len(extra))
		}
		st := s.router.Stats()
		if st.PacketsIn[InNACK].Load() != 1 || st.PacketsOut[OutVideoRTX].Load() != 3 || st.RTXMisses.Load() != 1 {
			t.Fatalf("nack in %d, rtx out %d, misses %d", st.PacketsIn[InNACK].Load(),
				st.PacketsOut[OutVideoRTX].Load(), st.RTXMisses.Load())
		}
	})
}

func TestRouterRejectsBadNACKs(t *testing.T) {
	s := newTestServer(t, func(c *room.Config) { c.P2PEnabled = false }, nil)
	a1 := s.peer("a1", 0)
	a2 := s.peer("a2", a1.client.InRoom())
	b1 := s.peer("b1", 0)
	b2 := s.peer("b2", b1.client.InRoom())
	for _, p := range []*peer{a1, a2, b1, b2} {
		p.register(s.addr)
	}
	pkt := a1.videoPacket(1)
	a1.send(s.addr, pkt)
	seq := uint16(pkt[2])<<8 | uint16(pkt[3])
	if got := a2.recv(300 * time.Millisecond); len(got) != 1 {
		t.Fatalf("a2 got %d packets", len(got))
	}
	st := s.router.Stats()
	for _, c := range []struct {
		name string
		from *peer
		nack []byte
		drop DropReason
	}{
		{"spoofed sender", b1, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, seq), DropAddrMismatch},
		{"unknown sender", a2, nackPacket(t, 0xDEAD, a1.joined.VideoSSRC, seq), DropUnknownSSRC},
		{"other room", b1, nackPacket(t, b1.joined.VideoSSRC, a1.joined.VideoSSRC, seq), DropBadPayload},
		{"own stream", a1, nackPacket(t, a1.joined.VideoSSRC, a1.joined.VideoSSRC, seq), DropBadPayload},
		{"audio stream", a2, nackPacket(t, a2.joined.VideoSSRC, a1.joined.AudioSSRC, seq), DropBadPayload},
		{"truncated", a2, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, seq)[:14], DropMalformed},
	} {
		before := st.Drops[c.drop].Load()
		c.from.send(s.addr, c.nack)
		waitFor(t, c.name, func() bool { return st.Drops[c.drop].Load() == before+1 })
	}
	for _, p := range []*peer{a1, a2, b1, b2} {
		if got := p.recv(100 * time.Millisecond); len(got) != 0 {
			t.Fatalf("%s received %d packets after bad NACKs", p.client.Name, len(got))
		}
	}
	if n := st.PacketsOut[OutVideoRTX].Load(); n != 0 {
		t.Fatalf("%d retransmissions", n)
	}
}

// A NACK counts against its sender's ingress budget: once that is spent, further
// NACKs are dropped (DropNACKLimited) and bring back nothing.
func TestRouterLimitsNACKsByIngressBudget(t *testing.T) {
	// Each client has its own bucket: room for a1's one video packet, and for two of
	// a2's NACKs but not a third.
	const burst = 100
	s := newTestServer(t, func(c *room.Config) { c.P2PEnabled = false }, func(c *Config) {
		c.IngressBytesPerSec = 1 // no refill to speak of during the test
		c.IngressBurstBytes = burst
	})
	a1 := s.peer("a1", 0)
	a2 := s.peer("a2", a1.client.InRoom())
	for _, p := range []*peer{a1, a2} {
		p.register(s.addr)
	}
	pkt := a1.videoPacket(1)
	video := len(pkt) + wireOverhead
	a1.send(s.addr, pkt)
	if got := a2.recv(300 * time.Millisecond); len(got) != 1 {
		t.Fatalf("a2 got %d packets", len(got))
	}
	seq := uint16(pkt[2])<<8 | uint16(pkt[3])
	req := nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, seq)
	nack := len(req) + wireOverhead
	if video > burst || 2*nack > burst || 3*nack <= burst {
		t.Fatalf("budget does not fit the packets: video %d B, NACK %d B", video, nack)
	}
	st := s.router.Stats()
	for i := range 3 {
		a2.send(s.addr, req)
		got := a2.recv(200 * time.Millisecond)
		if want := 1; i == 2 {
			want = 0
			if len(got) != want {
				t.Fatalf("NACK %d past the budget brought back %d packets", i+1, len(got))
			}
		} else if len(got) != want {
			t.Fatalf("NACK %d brought back %d packets, want %d", i+1, len(got), want)
		}
	}
	if d := st.Drops[DropNACKLimited].Load(); d != 1 {
		t.Fatalf("nack_limited drops = %d, want 1", d)
	}
	if n := st.PacketsOut[OutVideoRTX].Load(); n != 2 {
		t.Fatalf("%d retransmissions, want 2", n)
	}
}

// Retransmissions count against the requester's retransmission budget: once it is
// spent, the rest of the NACK is dropped (DropRTXBudget) and nothing more is resent.
func TestRouterStopsRetransmittingPastTheRTXBudget(t *testing.T) {
	s := newTestServer(t, func(c *room.Config) { c.P2PEnabled = false }, func(c *Config) {
		c.RTXBytesPerSec = 1  // no refill to speak of during the test
		c.RTXBurstBytes = 120 // two retransmissions of a ~50-byte packet, not three
	})
	a1 := s.peer("a1", 0)
	a2 := s.peer("a2", a1.client.InRoom())
	for _, p := range []*peer{a1, a2} {
		p.register(s.addr)
	}
	var seqs []uint16
	for i := range 5 {
		pkt := a1.videoPacket(uint32(i + 1))
		if c := len(pkt) + wireOverhead; 2*c > 120 || 3*c <= 120 {
			t.Fatalf("a retransmission costs %d B, which does not fit the test's budget", c)
		}
		seqs = append(seqs, uint16(pkt[2])<<8|uint16(pkt[3]))
		a1.send(s.addr, pkt)
	}
	if got := a2.recv(300 * time.Millisecond); len(got) != len(seqs) {
		t.Fatalf("a2 got %d packets", len(got))
	}
	st := s.router.Stats()
	a2.send(s.addr, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, seqs...))
	if got := a2.recv(300 * time.Millisecond); len(got) != 2 {
		t.Fatalf("a2 got %d retransmissions, want 2 (the budget)", len(got))
	}
	waitFor(t, "rtx budget drop", func() bool { return st.Drops[DropRTXBudget].Load() == 1 })
	// The budget is spent: a second NACK brings back nothing.
	a2.send(s.addr, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, seqs[2:]...))
	if got := a2.recv(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a2 got %d retransmissions past the budget", len(got))
	}
	waitFor(t, "second rtx budget drop", func() bool { return st.Drops[DropRTXBudget].Load() == 2 })
	if n := st.PacketsOut[OutVideoRTX].Load(); n != 2 {
		t.Fatalf("%d retransmissions, want 2", n)
	}
}

func TestRouterNACKDisabled(t *testing.T) {
	s := newTestServer(t, func(c *room.Config) { c.P2PEnabled = false }, func(c *Config) { c.NACKBufferPackets = 0 })
	a1 := s.peer("a1", 0)
	a2 := s.peer("a2", a1.client.InRoom())
	a1.register(s.addr)
	a2.register(s.addr)
	pkt := a1.videoPacket(1)
	a1.send(s.addr, pkt)
	if got := a2.recv(300 * time.Millisecond); len(got) != 1 {
		t.Fatalf("a2 got %d packets", len(got))
	}
	st := s.router.Stats()
	a2.send(s.addr, nackPacket(t, a2.joined.VideoSSRC, a1.joined.VideoSSRC, uint16(pkt[2])<<8|uint16(pkt[3])))
	waitFor(t, "the NACK to be dropped", func() bool { return st.Drops[DropBadPayload].Load() == 1 })
	if got := a2.recv(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a2 got %d retransmissions with NACK off", len(got))
	}
}

func TestRTXRingWrapsAndGuards(t *testing.T) {
	q := newRTXRing(100) // rounded up to 128
	if q.size != 128 {
		t.Fatalf("size %d", q.size)
	}
	pkt := func(seq uint16) []byte { return []byte{byte(seq >> 8), byte(seq), 0xAB} }
	for i := range 300 {
		seq := uint16(65400 + i) // wraps at 65536
		q.store(pkt(seq), seq)
	}
	base := uint16(65400)
	top := base + 299
	if b := q.get(top, 8); !bytes.Equal(b, pkt(top)) {
		t.Fatalf("newest packet: %x", b)
	}
	if b := q.get(top-119, 8); !bytes.Equal(b, pkt(top-119)) {
		t.Fatalf("oldest packet outside the guard: %x", b)
	}
	for _, seq := range []uint16{top - 120, top - 127, top - 200, top + 1} {
		if b := q.get(seq, 8); b != nil {
			t.Fatalf("seq %d (top %d) returned %x", seq, top, b)
		}
	}
	if b := q.get(top, 200); b != nil {
		t.Fatal("a guard larger than the ring still returned a packet")
	}
}

func TestRouterDropsSpoofedAndUnknownSSRC(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	b, c := s.peer("b", a.client.InRoom()), s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	attacker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	// a's SSRC from another address, and an SSRC nobody owns.
	if _, err := attacker.WriteToUDPAddrPort(a.videoPacket(1), s.addr); err != nil {
		t.Fatal(err)
	}
	bogus := a.videoPacket(2)
	binary.BigEndian.PutUint32(bogus[8:12], 0x12345678)
	a.send(s.addr, bogus)
	waitFor(t, "drops", func() bool {
		st := s.router.Stats().Snapshot()
		return st.Drops["addr_mismatch"] == 1 && st.Drops["unknown_ssrc"] == 1
	})
	for _, p := range []*peer{b, c} {
		if got := p.recv(100 * time.Millisecond); len(got) != 0 {
			t.Fatalf("spoofed packet forwarded to %s", p.client.Name)
		}
	}
}

func TestRouterRejectsBadToken(t *testing.T) {
	s := newTestServer(t, nil, nil)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reg := protocol.Register{TxID: 1}
	buf := make([]byte, protocol.RegisterSize)
	n, _ := reg.MarshalTo(buf)
	_, _ = conn.WriteToUDPAddrPort(buf[:n], s.addr)
	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("server acknowledged an unknown token")
	}
	if s.router.Stats().Snapshot().Drops["bad_token"] != 1 {
		t.Fatal("bad token not counted")
	}
	_, _ = conn.WriteToUDPAddrPort([]byte{0x42, 1, 2}, s.addr)
	waitFor(t, "malformed drop", func() bool { return s.router.Stats().Snapshot().Drops["malformed"] == 1 })
}

func TestRouterP2PRoomForwardsNothing(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	a.register(s.addr)
	b.register(s.addr)
	if s.reg.View().Rooms[0].Mode != room.ModeP2P {
		t.Fatal("two registered members should be in P2P mode")
	}
	a.send(s.addr, a.videoPacket(1))
	waitFor(t, "p2p drop", func() bool { return s.router.Stats().Snapshot().Drops["p2p_mode"] == 1 })
	if got := b.recv(100 * time.Millisecond); len(got) != 0 {
		t.Fatal("server relayed media in P2P mode")
	}
	if out := s.router.Stats().Snapshot().PacketsOut["video"]; out != 0 {
		t.Fatalf("video out = %d in P2P mode", out)
	}
}

func TestRouterRateLimitsSenders(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) {
		c.IngressBytesPerSec = 1000
		c.IngressBurstBytes = 1000
	})
	a := s.peer("a", 0)
	b, c := s.peer("b", a.client.InRoom()), s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	for i := 0; i < 40; i++ { // 40 x (~36+28) bytes, far above the 1000-byte burst
		a.send(s.addr, a.videoPacket(uint32(i)))
	}
	waitFor(t, "rate limit drops", func() bool {
		st := s.router.Stats().Snapshot()
		return st.Drops["ingress_limited"]+st.PacketsIn["video"] == 40
	})
	st := s.router.Stats().Snapshot()
	if st.Drops["ingress_limited"] == 0 || st.PacketsIn["video"] == 0 {
		t.Fatalf("expected some packets admitted and some dropped: %+v", st.Drops)
	}
}

// mixedFrames collects the mixed audio frames a peer receives: first sample and
// CSRC list per packet, plus the parity packets.
type mixedFrame struct {
	value  int16
	csrc   []uint32
	marker bool
}

func decodeMixed(t *testing.T, pkts [][]byte, mixer uint32) (frames []mixedFrame, parity int) {
	t.Helper()
	for _, pkt := range pkts {
		var h protocol.RTPHeader
		payload, err := protocol.ParseRTP(pkt, &h)
		if err != nil {
			t.Fatal(err)
		}
		if h.SSRC != mixer {
			t.Fatalf("mixed packet from SSRC %d, want mixer %d", h.SSRC, mixer)
		}
		if h.PayloadType == protocol.PTFEC {
			parity++
			continue
		}
		f := mixedFrame{value: int16(binary.BigEndian.Uint16(payload)), marker: h.Marker}
		for i := 0; i < int(h.CSRCCount); i++ {
			f.csrc = append(f.csrc, h.CSRC[i])
		}
		frames = append(frames, f)
	}
	return frames, parity
}

func TestRouterMixMinusOverTheWire(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) { c.AudioFECK = 2 })
	a := s.peer("a", 0)
	b, c := s.peer("b", a.client.InRoom()), s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	// a and b talk with constant sample values; c only listens.
	const frames = 10
	for i := 0; i < frames; i++ {
		ts := uint32(i * frameSamples)
		a.send(s.addr, audioPacket(uint16(i), ts, a.joined.AudioSSRC, 1000))
		b.send(s.addr, audioPacket(uint16(i), ts, b.joined.AudioSSRC, 2000))
		time.Sleep(MixInterval)
	}
	mixer := a.joined.MixerSSRC
	check := func(p *peer, want int16, wantCSRC []uint32) {
		t.Helper()
		got, parity := decodeMixed(t, p.recv(300*time.Millisecond), mixer)
		full := 0
		for _, f := range got {
			if f.value == want && len(f.csrc) == len(wantCSRC) {
				full++
				for i := range wantCSRC {
					if f.csrc[i] != wantCSRC[i] {
						t.Fatalf("%s: csrc %v, want %v", p.client.Name, f.csrc, wantCSRC)
					}
				}
			}
		}
		if full < frames/2 {
			t.Fatalf("%s: only %d of %d frames had the expected mix %d: %+v", p.client.Name, full, len(got), want, got)
		}
		if parity == 0 {
			t.Fatalf("%s: no parity packets with AudioFECK=2", p.client.Name)
		}
		if !got[0].marker {
			t.Fatalf("%s: first packet of a talk spurt must carry the marker bit", p.client.Name)
		}
	}
	check(c, 3000, []uint32{a.joined.AudioSSRC, b.joined.AudioSSRC})
	check(a, 2000, []uint32{b.joined.AudioSSRC})
	check(b, 1000, []uint32{a.joined.AudioSSRC})
}

func TestRouterMixSaturates(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) { c.AudioFECK = 0 })
	a := s.peer("a", 0)
	b, c := s.peer("b", a.client.InRoom()), s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	for i := 0; i < 8; i++ {
		ts := uint32(i * frameSamples)
		a.send(s.addr, audioPacket(uint16(i), ts, a.joined.AudioSSRC, 30000))
		b.send(s.addr, audioPacket(uint16(i), ts, b.joined.AudioSSRC, 30000))
		time.Sleep(MixInterval)
	}
	got, parity := decodeMixed(t, c.recv(300*time.Millisecond), a.joined.MixerSSRC)
	if parity != 0 {
		t.Fatal("parity sent with FEC disabled")
	}
	saturated := 0
	for _, f := range got {
		if f.value == 32767 {
			saturated++
		}
		if f.value < 0 {
			t.Fatalf("sum wrapped around to %d instead of saturating", f.value)
		}
	}
	if saturated < 4 {
		t.Fatalf("expected saturated frames, got %+v", got)
	}
}
