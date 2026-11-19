package room

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// inbox is a Notifier that decodes and records every frame it receives.
type inbox struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (b *inbox) Send(frame []byte) {
	body, err := protocol.ReadFrame(bytes.NewReader(frame), nil)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	b.mu.Unlock()
}

// take returns and clears the recorded messages.
func (b *inbox) take() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.msgs
	b.msgs = nil
	return out
}

func types(msgs []map[string]any) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m["type"].(string)
	}
	return out
}

func last(t *testing.T, msgs []map[string]any, typ string) map[string]any {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["type"] == typ {
			return msgs[i]
		}
	}
	t.Fatalf("no %q message in %v", typ, types(msgs))
	return nil
}

type fixture struct {
	t     *testing.T
	reg   *Registry
	now   time.Time
	views []*View
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	f := &fixture{t: t, now: time.Unix(1700000000, 0)}
	cfg := DefaultConfig()
	cfg.MediaPort = 6100
	cfg.Now = func() time.Time { return f.now }
	if mutate != nil {
		mutate(&cfg)
	}
	f.reg = NewRegistry(cfg)
	f.reg.Observe(func(v *View) { f.views = append(f.views, v) })
	return f
}

func (f *fixture) client(name string) (*Client, *inbox) {
	f.t.Helper()
	b := &inbox{}
	c, err := f.reg.Hello(name, b)
	if err != nil {
		f.t.Fatalf("Hello(%q): %v", name, err)
	}
	return c, b
}

func seq(n int64) *int64 { return &n }

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// tokenOf extracts the session token from the client's joined message.
func tokenOf(t *testing.T, msgs []map[string]any) protocol.Token {
	t.Helper()
	tok, err := protocol.ParseToken(last(t, msgs, protocol.TypeJoined)["token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestHelloValidatesNames(t *testing.T) {
	f := newFixture(t, nil)
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxNameLen+1), "a\x00b", "line\nbreak"} {
		if _, err := f.reg.Hello(bad, &inbox{}); code(err) != protocol.CodeBadRequest {
			t.Errorf("Hello(%q) err = %v, want bad_request", bad, err)
		}
	}
	a, _ := f.client("  alice ")
	b, _ := f.client("Zoë 你好")
	if a.Name != "alice" || b.ID != a.ID+1 {
		t.Fatalf("unexpected clients %+v %+v", a, b)
	}
}

func TestCreateJoinListLeave(t *testing.T) {
	f := newFixture(t, nil)
	alice, ain := f.client("alice")
	bob, bin := f.client("bob")

	if err := f.reg.CreateRoom(alice, "standup", seq(1)); err != nil {
		t.Fatal(err)
	}
	msgs := ain.take()
	if got := types(msgs); got[0] != protocol.TypeJoined {
		t.Fatalf("joined must come first, got %v", got)
	}
	joined := msgs[0]
	if joined["seq"] != float64(1) || joined["host"] != true || joined["media_port"] != float64(6100) {
		t.Fatalf("bad joined %v", joined)
	}
	if m := last(t, msgs, protocol.TypeMode); m["mode"] != protocol.ModeRelay {
		t.Fatalf("a one-member room must be relay, got %v", m)
	}
	roomID := alice.InRoom()

	rooms := f.reg.ListRooms()
	if len(rooms) != 1 || rooms[0].Title != "standup" || rooms[0].HostName != "alice" || rooms[0].Participants != 1 {
		t.Fatalf("ListRooms = %+v", rooms)
	}

	if err := f.reg.JoinRoom(bob, roomID, seq(7)); err != nil {
		t.Fatal(err)
	}
	bmsgs := bin.take()
	if types(bmsgs)[0] != protocol.TypeJoined || bmsgs[0]["host"] != false || bmsgs[0]["seq"] != float64(7) {
		t.Fatalf("bob's first message %v", bmsgs[0])
	}
	if last(t, bmsgs, protocol.TypeMode)["mode"] != protocol.ModeRelay {
		t.Fatal("bob must learn the current mode on join")
	}
	roster := last(t, ain.take(), protocol.TypeRoster)
	if n := len(roster["participants"].([]any)); n != 2 {
		t.Fatalf("alice's roster has %d members", n)
	}

	if err := f.reg.LeaveRoom(bob, seq(8)); err != nil {
		t.Fatal(err)
	}
	if left := last(t, bin.take(), protocol.TypeLeft); left["seq"] != float64(8) {
		t.Fatalf("left = %v", left)
	}
	if roster := last(t, ain.take(), protocol.TypeRoster); len(roster["participants"].([]any)) != 1 {
		t.Fatal("alice's roster after bob left")
	}
	if bob.InRoom() != 0 || len(f.reg.ListRooms()) != 1 {
		t.Fatal("room must survive a non-host leaving")
	}

	// The room closes automatically when the last member leaves.
	if err := f.reg.LeaveRoom(alice, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.reg.ListRooms()) != 0 {
		t.Fatal("empty room still listed")
	}
	if err := f.reg.JoinRoom(bob, roomID, nil); code(err) != protocol.CodeRoomNotFound {
		t.Fatalf("joining a closed room: %v", err)
	}
}

func TestRequestErrors(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxRoomSize = 2 })
	a, _ := f.client("a")
	b, _ := f.client("b")
	c, _ := f.client("c")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"leave outside room", f.reg.LeaveRoom(a, nil), protocol.CodeNotInRoom},
		{"cancel outside room", f.reg.CancelRoom(a, nil), protocol.CodeNotInRoom},
		{"chat outside room", f.reg.Chat(a, "hi"), protocol.CodeNotInRoom},
		{"media state outside room", f.reg.SetMediaState(a, true, true), protocol.CodeNotInRoom},
		{"join missing room", f.reg.JoinRoom(a, 99, nil), protocol.CodeRoomNotFound},
		{"title too long", f.reg.CreateRoom(a, strings.Repeat("t", MaxTitleLen+1), nil), protocol.CodeBadRequest},
	}
	for _, tt := range tests {
		if code(tt.err) != tt.want {
			t.Errorf("%s: err = %v, want %s", tt.name, tt.err, tt.want)
		}
	}
	if err := f.reg.CreateRoom(a, "", nil); err != nil {
		t.Fatal(err)
	}
	id := a.InRoom()
	if err := f.reg.CreateRoom(a, "", nil); code(err) != protocol.CodeAlreadyInRoom {
		t.Errorf("second create: %v", err)
	}
	if err := f.reg.JoinRoom(b, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.JoinRoom(c, id, nil); code(err) != protocol.CodeRoomFull {
		t.Errorf("join full room: %v", err)
	}
	if err := f.reg.Chat(a, ""); code(err) != protocol.CodeBadRequest {
		t.Errorf("empty chat: %v", err)
	}
	if err := f.reg.Chat(a, strings.Repeat("x", MaxChatLen+1)); code(err) != protocol.CodeBadRequest {
		t.Errorf("long chat: %v", err)
	}
	if rooms := f.reg.ListRooms(); rooms[0].Title != "a's room" {
		t.Errorf("default title = %q", rooms[0].Title)
	}
}

func TestServerLimits(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxRooms = 1; c.MaxClients = 2 })
	a, _ := f.client("a")
	b, _ := f.client("b")
	if _, err := f.reg.Hello("c", &inbox{}); code(err) != protocol.CodeLimitReached {
		t.Fatalf("third client: %v", err)
	}
	if err := f.reg.CreateRoom(a, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CreateRoom(b, "", nil); code(err) != protocol.CodeLimitReached {
		t.Fatalf("second room: %v", err)
	}
	f.reg.Goodbye(b)
	if _, err := f.reg.Hello("c", &inbox{}); err != nil {
		t.Fatalf("slot not freed after Goodbye: %v", err)
	}
}

func TestCancelOnlyByHost(t *testing.T) {
	f := newFixture(t, nil)
	host, hin := f.client("host")
	p1, p1in := f.client("p1")
	p2, p2in := f.client("p2")
	if err := f.reg.CreateRoom(host, "", nil); err != nil {
		t.Fatal(err)
	}
	id := host.InRoom()
	for _, c := range []*Client{p1, p2} {
		if err := f.reg.JoinRoom(c, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.reg.CancelRoom(p1, nil); code(err) != protocol.CodeNotHost {
		t.Fatalf("participant cancel: %v", err)
	}
	hin.take()
	p1in.take()
	p2in.take()
	if err := f.reg.CancelRoom(host, seq(3)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]*inbox{"host": hin, "p1": p1in, "p2": p2in} {
		msgs := in.take()
		if len(msgs) != 1 || msgs[0]["type"] != protocol.TypeRoomClosed || msgs[0]["reason"] != protocol.ReasonCancelled {
			t.Fatalf("%s got %v", name, msgs)
		}
		if _, hasSeq := msgs[0]["seq"]; hasSeq != (name == "host") {
			t.Fatalf("%s: seq must only be echoed to the requester: %v", name, msgs[0])
		}
	}
	if host.InRoom() != 0 || p1.InRoom() != 0 || len(f.reg.ListRooms()) != 0 {
		t.Fatal("members not removed by cancel")
	}
	if err := f.reg.JoinRoom(p2, id, nil); code(err) != protocol.CodeRoomNotFound {
		t.Fatal("cancelled room must not accept joins")
	}
	if err := f.reg.CreateRoom(p1, "", nil); err != nil || p1.InRoom() == id {
		t.Fatalf("new room after cancel: %v (id %d)", err, p1.InRoom())
	}
}

func TestHostLeavingKeepsRoomWithoutHost(t *testing.T) {
	f := newFixture(t, nil)
	host, _ := f.client("host")
	p, pin := f.client("p")
	_ = f.reg.CreateRoom(host, "", nil)
	_ = f.reg.JoinRoom(p, host.InRoom(), nil)
	f.reg.Goodbye(host) // disconnect without leave
	roster := last(t, pin.take(), protocol.TypeRoster)
	if roster["host_id"] != float64(0) || len(roster["participants"].([]any)) != 1 {
		t.Fatalf("roster after host left: %v", roster)
	}
	if err := f.reg.CancelRoom(p, nil); code(err) != protocol.CodeNotHost {
		t.Fatalf("non-host cancel after host left: %v", err)
	}
	if len(f.reg.ListRooms()) != 1 {
		t.Fatal("room must continue after the host leaves")
	}
}

func TestChatIsStampedAndIsolated(t *testing.T) {
	f := newFixture(t, nil)
	a, ain := f.client("alice")
	b, bin := f.client("bob")
	c, cin := f.client("carol") // in another room
	_ = f.reg.CreateRoom(a, "", nil)
	_ = f.reg.JoinRoom(b, a.InRoom(), nil)
	_ = f.reg.CreateRoom(c, "", nil)
	ain.take()
	bin.take()
	cin.take()

	f.now = f.now.Add(1500 * time.Millisecond)
	if err := f.reg.Chat(b, "  hello\nworld  "); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]*inbox{"alice": ain, "bob": bin} {
		msgs := in.take()
		if len(msgs) != 1 {
			t.Fatalf("%s got %v", name, msgs)
		}
		m := msgs[0]
		if m["from_name"] != "bob" || m["from_id"] != float64(b.ID) || m["text"] != "hello\nworld" ||
			m["server_time_ms"] != float64(f.now.UnixMilli()) {
			t.Fatalf("%s got %v", name, m)
		}
	}
	if msgs := cin.take(); len(msgs) != 0 {
		t.Fatalf("chat leaked into another room: %v", msgs)
	}
}

func TestDecideMode(t *testing.T) {
	a := netip.MustParseAddrPort("127.0.0.1:1000")
	b := netip.MustParseAddrPort("127.0.0.1:1001")
	var none netip.AddrPort
	tests := []struct {
		name    string
		members int
		addrs   []netip.AddrPort
		p2p     bool
		want    Mode
	}{
		{"alone", 1, []netip.AddrPort{a}, true, ModeRelay},
		{"two registered", 2, []netip.AddrPort{a, b}, true, ModeP2P},
		{"two, one unregistered", 2, []netip.AddrPort{a, none}, true, ModeRelay},
		{"two, p2p disabled", 2, []netip.AddrPort{a, b}, false, ModeRelay},
		{"three", 3, []netip.AddrPort{a, b, a}, true, ModeRelay},
	}
	for _, tt := range tests {
		if got := DecideMode(tt.members, tt.addrs, tt.p2p); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

func TestModeSwitchingP2PRelayP2P(t *testing.T) {
	f := newFixture(t, nil)
	a, ain := f.client("a")
	b, bin := f.client("b")
	c, cin := f.client("c")
	addrA := netip.MustParseAddrPort("127.0.0.1:40001")
	addrB := netip.MustParseAddrPort("127.0.0.1:40002")
	addrC := netip.MustParseAddrPort("127.0.0.1:40003")

	_ = f.reg.CreateRoom(a, "", nil)
	tokA := tokenOf(t, ain.take())
	_ = f.reg.JoinRoom(b, a.InRoom(), nil)
	tokB := tokenOf(t, bin.take())
	ain.take()

	if f.reg.Register(protocol.Token{}, addrA) {
		t.Fatal("unknown token accepted")
	}
	if !f.reg.Register(tokA, addrA) {
		t.Fatal("valid token rejected")
	}
	for _, m := range ain.take() {
		if m["type"] == protocol.TypeMode {
			t.Fatal("mode must stay relay until both members registered")
		}
	}
	bin.take()
	if !f.reg.Register(tokB, addrB) {
		t.Fatal("valid token rejected")
	}
	modeA := last(t, ain.take(), protocol.TypeMode)
	modeB := last(t, bin.take(), protocol.TypeMode)
	peerA := modeA["peer"].(map[string]any)
	peerB := modeB["peer"].(map[string]any)
	if modeA["mode"] != protocol.ModeP2P || peerA["addr"] != addrB.String() || peerB["addr"] != addrA.String() {
		t.Fatalf("p2p announcement wrong: %v / %v", modeA, modeB)
	}
	if peerA["participant_id"] != float64(b.ID) {
		t.Fatalf("a's peer is %v", peerA)
	}
	v := f.views[len(f.views)-1]
	if v.Rooms[0].Mode != ModeP2P || v.Rooms[0].Relaying(f.now.Add(2*time.Second)) {
		t.Fatalf("view not p2p: %+v", v.Rooms[0])
	}
	if !v.Rooms[0].Relaying(f.now) {
		t.Fatal("relay grace period missing after relay -> p2p")
	}

	// A third member switches the room to relay.
	_ = f.reg.JoinRoom(c, a.InRoom(), nil)
	tokC := tokenOf(t, cin.take())
	for name, in := range map[string]*inbox{"a": ain, "b": bin} {
		if m := last(t, in.take(), protocol.TypeMode); m["mode"] != protocol.ModeRelay || m["peer"] != nil {
			t.Fatalf("%s: expected relay, got %v", name, m)
		}
	}
	_ = f.reg.Register(tokC, addrC)
	if v := f.views[len(f.views)-1]; v.Rooms[0].Mode != ModeRelay || len(v.Rooms[0].Members) != 3 {
		t.Fatalf("view after third join: %+v", v.Rooms[0])
	}

	// Back to two members: P2P again, with a fresh relay grace period.
	f.now = f.now.Add(10 * time.Second)
	_ = f.reg.LeaveRoom(c, nil)
	if m := last(t, ain.take(), protocol.TypeMode); m["mode"] != protocol.ModeP2P {
		t.Fatalf("expected p2p after leave, got %v", m)
	}
	rv := f.views[len(f.views)-1].Rooms[0]
	if !rv.Relaying(f.now.Add(999*time.Millisecond)) || rv.Relaying(f.now.Add(time.Second)) {
		t.Fatalf("grace period wrong: until %v (now %v)", rv.RelayUntil, f.now)
	}

	// A NAT rebinding (new source address) is re-announced to the peer.
	newB := netip.MustParseAddrPort("127.0.0.1:40009")
	bin.take()
	_ = f.reg.Register(tokB, newB)
	if m := last(t, ain.take(), protocol.TypeMode); m["peer"].(map[string]any)["addr"] != newB.String() {
		t.Fatalf("rebinding not announced: %v", m)
	}
}

func TestP2PDisabledAlwaysRelays(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.P2PEnabled = false })
	a, ain := f.client("a")
	b, bin := f.client("b")
	_ = f.reg.CreateRoom(a, "", nil)
	tokA := tokenOf(t, ain.take())
	_ = f.reg.JoinRoom(b, a.InRoom(), nil)
	tokB := tokenOf(t, bin.take())
	f.reg.Register(tokA, netip.MustParseAddrPort("127.0.0.1:1"))
	f.reg.Register(tokB, netip.MustParseAddrPort("127.0.0.1:2"))
	if f.views[len(f.views)-1].Rooms[0].Mode != ModeRelay {
		t.Fatal("p2p used although disabled")
	}
}

func TestViewTracksMembershipAndState(t *testing.T) {
	f := newFixture(t, nil)
	a, ain := f.client("a")
	_ = f.reg.CreateRoom(a, "", nil)
	tok := tokenOf(t, ain.take())
	addr := netip.MustParseAddrPort("[::1]:5000")
	f.reg.Register(tok, addr)
	_ = f.reg.SetMediaState(a, false, true)
	v := f.reg.View()
	if len(v.Rooms) != 1 {
		t.Fatalf("rooms = %d", len(v.Rooms))
	}
	m := v.Rooms[0].Members[0]
	if m.ID != a.ID || m.Addr != addr || m.Video || !m.Audio || m.AudioSSRC == m.VideoSSRC || m.AudioSSRC == 0 {
		t.Fatalf("member view %+v", m)
	}
	if rooms, members := v.Stats(); rooms != 1 || members != 1 {
		t.Fatalf("Stats = %d, %d", rooms, members)
	}
	f.reg.Goodbye(a)
	if rooms, _ := f.reg.View().Stats(); rooms != 0 {
		t.Fatal("room not closed after its only member disconnected")
	}
	// The old token must not bind anything any more.
	if f.reg.Register(tok, addr) {
		t.Fatal("token of a departed member accepted")
	}
}

func TestSSRCsAreUnique(t *testing.T) {
	f := newFixture(t, nil)
	seen := map[uint32]bool{}
	host, _ := f.client("host")
	_ = f.reg.CreateRoom(host, "", nil)
	for i := 0; i < 15; i++ {
		c, _ := f.client("p")
		if err := f.reg.JoinRoom(c, host.InRoom(), nil); err != nil {
			t.Fatal(err)
		}
	}
	rv := f.reg.View().Rooms[0]
	seen[rv.MixerSSRC] = true
	for _, m := range rv.Members {
		for _, s := range []uint32{m.AudioSSRC, m.VideoSSRC} {
			if seen[s] {
				t.Fatalf("duplicate SSRC %d", s)
			}
			seen[s] = true
		}
	}
}

func TestCloseAll(t *testing.T) {
	f := newFixture(t, nil)
	a, ain := f.client("a")
	_ = f.reg.CreateRoom(a, "", nil)
	ain.take()
	f.reg.CloseAll()
	if m := last(t, ain.take(), protocol.TypeRoomClosed); m["reason"] != protocol.ReasonServerShutdown {
		t.Fatalf("got %v", m)
	}
	if len(f.reg.ListRooms()) != 0 {
		t.Fatal("rooms left after CloseAll")
	}
}
