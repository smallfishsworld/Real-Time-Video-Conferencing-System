package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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
	capt.mu.Lock()
	p := &peer{t: s.t, client: c, joined: capt.joined, conn: conn}
	capt.mu.Unlock()
	return p
}

// register performs the REGISTER / REGISTER_ACK exchange.
func (p *peer) register(server netip.AddrPort) {
	p.t.Helper()
	tok, err := protocol.ParseToken(p.joined.Token)
	if err != nil {
		p.t.Fatal(err)
	}
	reg := protocol.Register{TxID: 99, Token: tok}
	buf := make([]byte, protocol.RegisterSize)
	n, _ := reg.MarshalTo(buf)
	if _, err := p.conn.WriteToUDPAddrPort(buf[:n], server); err != nil {
		p.t.Fatal(err)
	}
	ackBuf := make([]byte, 64)
	_ = p.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := p.conn.Read(ackBuf)
	if err != nil {
		p.t.Fatalf("no REGISTER_ACK: %v", err)
	}
	var ack protocol.RegisterAck
	if err := protocol.ParseRegisterAck(ackBuf[:m], &ack); err != nil || ack.TxID != 99 {
		p.t.Fatalf("bad ack %x: %v", ackBuf[:m], err)
	}
	if ack.Observed != p.conn.LocalAddr().(*net.UDPAddr).AddrPort() {
		p.t.Fatalf("observed %v, local %v", ack.Observed, p.conn.LocalAddr())
	}
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

func TestRouterForwardsVideoOnlyWithinRoom(t *testing.T) {
	s := newTestServer(t, nil, nil)
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
		return st.Drops["rate_limited"]+st.PacketsIn["video"] == 40
	})
	st := s.router.Stats().Snapshot()
	if st.Drops["rate_limited"] == 0 || st.PacketsIn["video"] == 0 {
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
