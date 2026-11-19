package media

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// rawRegister sends one REGISTER from conn and returns the REGISTER_ACK that answers
// it, or ok=false when none arrives within 300 ms.
func rawRegister(t *testing.T, conn *net.UDPConn, server netip.AddrPort, reg protocol.Register) (protocol.RegisterAck, bool) {
	t.Helper()
	buf := make([]byte, protocol.RegisterSize)
	n, _ := reg.MarshalTo(buf)
	if _, err := conn.WriteToUDPAddrPort(buf[:n], server); err != nil {
		t.Fatal(err)
	}
	in := make([]byte, 128)
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		_ = conn.SetReadDeadline(deadline)
		m, err := conn.Read(in)
		if err != nil {
			return protocol.RegisterAck{}, false
		}
		var ack protocol.RegisterAck
		if protocol.ParseRegisterAck(in[:m], &ack) == nil && ack.TxID == reg.TxID {
			return ack, true
		}
	}
}

func tokenOf(t *testing.T, p *peer) protocol.Token {
	t.Helper()
	tok, err := protocol.ParseToken(p.joined.Token)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRegisterNeedsReturnRoutability(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	tok := tokenOf(t, a)
	// Without a nonce the server only challenges; nothing is bound.
	ack, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: 1, Token: tok})
	if !ok || !ack.Challenge {
		t.Fatalf("first REGISTER: ack %+v ok %v, want a challenge", ack, ok)
	}
	if m := s.reg.View().Rooms[0].Members[0]; m.Addr.IsValid() {
		t.Fatalf("bound to %v before the nonce was echoed", m.Addr)
	}
	// A wrong nonce is challenged again.
	bad := ack.Nonce
	bad[0] ^= 1
	if ack2, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: 2, Token: tok, HasNonce: true, Nonce: bad}); !ok || !ack2.Challenge {
		t.Fatalf("wrong nonce: ack %+v ok %v, want a challenge", ack2, ok)
	}
	// The same nonce from another address does not bind either: it is bound to the
	// address it was sent to.
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ack3, ok := rawRegister(t, other, s.addr, protocol.Register{TxID: 3, Token: tok, HasNonce: true, Nonce: ack.Nonce}); !ok || !ack3.Challenge {
		t.Fatalf("nonce from another address: ack %+v ok %v, want a challenge", ack3, ok)
	}
	// Echoing it from the challenged address binds.
	final, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: 4, Token: tok, HasNonce: true, Nonce: ack.Nonce})
	if !ok || final.Challenge {
		t.Fatalf("echo: ack %+v ok %v, want the final ACK", final, ok)
	}
	local := a.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	if m := s.reg.View().Rooms[0].Members[0]; m.Addr != local || final.Observed != local {
		t.Fatalf("bound to %v (observed %v), want %v", m.Addr, final.Observed, local)
	}
	// A keepalive from the bound address is answered at once, without a challenge.
	if ka, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: 5, Token: tok}); !ok || ka.Challenge {
		t.Fatalf("keepalive: ack %+v ok %v", ka, ok)
	}
	st := s.router.Stats().Snapshot()
	if st.PacketsOut["register_challenge"] != 3 || s.router.Stats().Bindings.Load() != 1 {
		t.Fatalf("challenges %d, bindings %d", st.PacketsOut["register_challenge"], s.router.Stats().Bindings.Load())
	}
}

func TestRebindMovesMediaAfterChallenge(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	c := s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	// b moves to a new socket, as after a NAT rebinding.
	moved, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if _, challenges, err := registerConn(moved, s.addr, b.joined.Token, 7); err != nil || challenges != 1 {
		t.Fatalf("rebind: %d challenges, %v", challenges, err)
	}
	a.send(s.addr, a.videoPacket(1))
	buf := make([]byte, 2048)
	_ = moved.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := moved.Read(buf); err != nil {
		t.Fatalf("no video at the new address: %v", err)
	}
	if got := b.recv(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("old address still gets %d datagrams", len(got))
	}
}

func TestUnknownTokensDoNotUseTheRegisterBudget(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) { c.RegistersPerSec = 5 })
	a := s.peer("a", 0)
	junk, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer junk.Close()
	pkt := make([]byte, protocol.RegisterSize)
	reg := protocol.Register{TxID: 1, Token: protocol.Token{1, 2, 3}}
	n, _ := reg.MarshalTo(pkt)
	for range 200 {
		_, _ = junk.WriteToUDPAddrPort(pkt[:n], s.addr)
	}
	waitFor(t, "bad tokens", func() bool { return s.router.Stats().Snapshot().Drops["bad_token"] == 200 })
	a.register(s.addr) // still within the server-wide budget of 5/s
	if d := s.router.Stats().Snapshot().Drops["register_limited"]; d != 0 {
		t.Fatalf("register_limited = %d", d)
	}
}

func TestRegisterFloodIsLimitedPerMember(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	a.register(s.addr)
	tok := tokenOf(t, a)
	pkt := make([]byte, protocol.RegisterSize)
	reg := protocol.Register{TxID: 1, Token: tok}
	n, _ := reg.MarshalTo(pkt)
	for range 200 {
		a.send(s.addr, pkt[:n])
	}
	waitFor(t, "member limit", func() bool { return s.router.Stats().Snapshot().Drops["register_limited"] >= 150 })
	// Another member is unaffected.
	b.register(s.addr)
}

func TestRebindsAreRateLimitedPerMember(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) { c.RebindsPerSec, c.RebindBurst = 0.001, 2 })
	a := s.peer("a", 0)
	a.register(s.addr) // the first binding uses one of the two
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, _, err := registerConn(other, s.addr, a.joined.Token, 2); err != nil {
		t.Fatalf("second binding: %v", err)
	}
	if _, _, err := registerConn(a.conn, s.addr, a.joined.Token, 3); err == nil {
		t.Fatal("third binding within the burst of two was accepted")
	}
	if d := s.router.Stats().Snapshot().Drops["rebind_limited"]; d == 0 {
		t.Fatal("no rebind_limited drops")
	}
}

func TestBindingExpiresWithoutKeepalives(t *testing.T) {
	s := newTestServer(t, nil, func(c *Config) { c.BindingTimeout = 300 * time.Millisecond })
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	a.register(s.addr)
	b.register(s.addr)
	tokA := tokenOf(t, a)
	// a keeps its binding with keepalives; b sends none.
	stop := time.Now().Add(900 * time.Millisecond)
	for i := uint64(10); time.Now().Before(stop); i++ {
		if ack, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: i, Token: tokA}); !ok || ack.Challenge {
			t.Fatalf("keepalive %d: %+v %v", i, ack, ok)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, "b's binding to expire", func() bool {
		m := s.reg.View().Rooms[0].Members
		return m[0].Addr.IsValid() && !m[1].Addr.IsValid()
	})
	if s.router.Stats().BindingsExpired.Load() != 1 {
		t.Fatalf("expired = %d", s.router.Stats().BindingsExpired.Load())
	}
	// b can bind again; the server challenges it like any new address.
	if _, challenges, err := registerConn(b.conn, s.addr, b.joined.Token, 99); err != nil || challenges != 1 {
		t.Fatalf("re-register: %d challenges, %v", challenges, err)
	}
}

// blockingNotifier blocks the registry (which calls Send with its lock held) until
// release is closed.
type blockingNotifier struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (n *blockingNotifier) Send([]byte) {
	n.once.Do(func() { close(n.entered) })
	<-n.release
}

// While the registry lock is held, keepalives are still answered and video is still
// forwarded: the Serve goroutine never takes it.
func TestServeDoesNotWaitForTheRegistry(t *testing.T) {
	s := newTestServer(t, nil, nil)
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	c := s.peer("c", a.client.InRoom())
	for _, p := range []*peer{a, b, c} {
		p.register(s.addr)
	}
	// A member of another room whose control connection blocks.
	n := &blockingNotifier{entered: make(chan struct{}), release: make(chan struct{})}
	cl, err := s.reg.Hello("stuck", n)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.reg.CreateRoom(cl, "", nil) }() // blocks inside, holding the lock
	<-n.entered
	defer close(n.release)
	tok := tokenOf(t, a)
	if ack, ok := rawRegister(t, a.conn, s.addr, protocol.Register{TxID: 5, Token: tok}); !ok || ack.Challenge {
		t.Fatalf("keepalive while the registry is locked: %+v %v", ack, ok)
	}
	a.send(s.addr, a.videoPacket(1))
	buf := make([]byte, 2048)
	_ = b.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.conn.Read(buf); err != nil {
		t.Fatalf("no video while the registry is locked: %v", err)
	}
}

// Leaving and rejoining keeps the client's ingress budget instead of refilling it.
func TestIngressBudgetSurvivesRejoin(t *testing.T) {
	s := newTestServer(t, func(rc *room.Config) { rc.P2PEnabled = false }, func(c *Config) {
		c.IngressBytesPerSec = 100
		c.IngressBurstBytes = 1000
	})
	a := s.peer("a", 0)
	b := s.peer("b", a.client.InRoom())
	a.register(s.addr)
	b.register(s.addr)
	for i := range 40 { // far above the 1000-byte burst
		a.send(s.addr, a.videoPacket(uint32(i)))
	}
	waitFor(t, "first drops", func() bool {
		st := s.router.Stats().Snapshot()
		return st.Drops["ingress_limited"]+st.PacketsIn["video"] == 40
	})
	admitted := s.router.Stats().Snapshot().PacketsIn["video"]
	if err := s.reg.LeaveRoom(b.client, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.LeaveRoom(a.client, nil); err != nil {
		t.Fatal(err)
	}
	// a rejoins (a new membership of the same client) with b in a new room.
	a2 := rejoin(t, s, a, 0)
	b2 := rejoin(t, s, b, a2.client.InRoom())
	a2.register(s.addr)
	b2.register(s.addr)
	for i := range 10 {
		a2.send(s.addr, a2.videoPacket(uint32(100+i)))
	}
	waitFor(t, "second drops", func() bool {
		st := s.router.Stats().Snapshot()
		return st.Drops["ingress_limited"]+st.PacketsIn["video"] == 50
	})
	if again := s.router.Stats().Snapshot().PacketsIn["video"] - admitted; again > 1 {
		t.Fatalf("%d packets admitted after the rejoin; the budget was refilled", again)
	}
}

// rejoin puts p's client into room roomID (0 creates a room) and returns the new
// membership with a fresh socket.
func rejoin(t *testing.T, s *testServer, p *peer, roomID uint32) *peer {
	t.Helper()
	// The joined message goes to the client's original notifier, so the new token and
	// SSRCs are read from the view.
	var err error
	if roomID == 0 {
		err = s.reg.CreateRoom(p.client, "", nil)
	} else {
		err = s.reg.JoinRoom(p.client, roomID, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	s.router.Sync()
	var mv room.MemberView
	for _, rv := range s.reg.View().Rooms {
		for _, m := range rv.Members {
			if m.ID == p.client.ID {
				mv = m
			}
		}
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	j := p.joined
	j.Token, j.AudioSSRC, j.VideoSSRC = mv.Token.String(), mv.AudioSSRC, mv.VideoSSRC
	return &peer{t: t, client: p.client, joined: j, conn: conn}
}
