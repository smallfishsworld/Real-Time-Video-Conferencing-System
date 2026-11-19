package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/client"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/media"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// The tests in this file start the whole server in-process on ephemeral loopback
// ports and drive it with fake clients over real TCP and UDP sockets.

const relayGrace = 150 * time.Millisecond

func startServer(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.MediaAddr = "127.0.0.1:0"
	cfg.MetricsAddr = "127.0.0.1:0"
	cfg.Room.RelayGrace = relayGrace
	srv, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return c
}

type member struct {
	t      *testing.T
	name   string
	ctl    *client.Control
	joined client.Message
	media  *client.Media
	vseq   uint16
	aseq   uint16
}

func dial(t *testing.T, srv *Server, name string) *client.Control {
	t.Helper()
	c, err := client.Dial(ctx(t), srv.ControlAddr(), name)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// enter connects name, creates (roomID 0) or joins a room and registers its media
// socket.
func enter(t *testing.T, srv *Server, name string, roomID uint32) *member {
	t.Helper()
	m := &member{t: t, name: name, ctl: dial(t, srv, name)}
	var err error
	if roomID == 0 {
		m.joined, err = m.ctl.Request(ctx(t), map[string]any{"type": "create_room", "title": name + "'s meeting"})
	} else {
		m.joined, err = m.ctl.Request(ctx(t), map[string]any{"type": "join_room", "room_id": roomID})
	}
	if err != nil {
		t.Fatalf("%s enter room %d: %v", name, roomID, err)
	}
	m.media, err = client.ListenMedia("127.0.0.1:0", netip.MustParseAddrPort(srv.MediaAddr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.media.Close() })
	if _, err := m.media.Register(m.joined.Token, 50*time.Millisecond, 2*time.Second); err != nil {
		t.Fatalf("%s register: %v", name, err)
	}
	return m
}

func (m *member) video(frameID uint32) []byte {
	p := client.Packetizer{SSRC: m.joined.VideoSSRC, Seq: m.vseq}
	pkts := p.VideoFrame(nil, []byte("tiny-jpeg-"+m.name), frameID, frameID*6000, 0, nil)
	m.vseq = p.Seq
	return pkts[0]
}

func (m *member) audio(ts uint32, v int16) []byte {
	pcm := make([]byte, protocol.AudioFrameBytes)
	for i := 0; i < protocol.AudioSamplesPerFrame; i++ {
		binary.BigEndian.PutUint16(pcm[2*i:], uint16(v))
	}
	p := client.Packetizer{SSRC: m.joined.AudioSSRC, Seq: m.aseq}
	pkts := p.AudioFrame(nil, pcm, ts, false, 0, nil)
	m.aseq = p.Seq
	return pkts[0]
}

func (m *member) sendTo(dst netip.AddrPort, pkt []byte) {
	m.t.Helper()
	if _, err := m.media.Conn.WriteToUDPAddrPort(pkt, dst); err != nil {
		m.t.Fatal(err)
	}
}

func (m *member) sendServer(pkt []byte) { m.sendTo(m.media.Server, pkt) }

type received struct {
	from netip.AddrPort
	hdr  protocol.RTPHeader
	body []byte
}

// recv collects RTP packets arriving within d.
func (m *member) recv(d time.Duration) []received {
	var out []received
	deadline := time.Now().Add(d)
	buf := make([]byte, 2048)
	for {
		_ = m.media.Conn.SetReadDeadline(deadline)
		n, from, err := m.media.Conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return out
		}
		var r received
		payload, err := protocol.ParseRTP(buf[:n], &r.hdr)
		if err != nil {
			continue
		}
		r.from = from
		r.body = append([]byte(nil), payload...)
		out = append(out, r)
	}
}

func (m *member) waitMode(t *testing.T, mode string) client.Message {
	t.Helper()
	msg, err := m.ctl.WaitEvent(ctx(t), protocol.TypeMode, func(x client.Message) bool { return x.Mode == mode })
	if err != nil {
		t.Fatalf("%s waiting for mode %s: %v", m.name, mode, err)
	}
	return msg
}

func countFrom(pkts []received, ssrc uint32) int {
	n := 0
	for _, p := range pkts {
		if p.hdr.SSRC == ssrc {
			n++
		}
	}
	return n
}

func TestMeetingLifecycle(t *testing.T) {
	srv := startServer(t)
	alice := dial(t, srv, "alice")
	created, err := alice.Request(ctx(t), map[string]any{"type": "create_room", "title": "standup"})
	if err != nil || !created.Host || created.Token == "" || created.MediaPort == 0 {
		t.Fatalf("create: %+v %v", created, err)
	}
	roomID := created.RoomID

	bob := dial(t, srv, "bob")
	list, err := bob.Request(ctx(t), map[string]any{"type": "list_rooms"})
	if err != nil || len(list.Rooms) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if r := list.Rooms[0]; r.RoomID != roomID || r.Title != "standup" || r.HostName != "alice" || r.Participants != 1 || r.Mode != "relay" {
		t.Fatalf("room info %+v", r)
	}
	joined, err := bob.Request(ctx(t), map[string]any{"type": "join_room", "room_id": roomID})
	if err != nil || joined.Host || joined.RoomID != roomID {
		t.Fatalf("join: %+v %v", joined, err)
	}
	if _, err := alice.WaitEvent(ctx(t), protocol.TypeRoster, func(m client.Message) bool { return len(m.Participants) == 2 }); err != nil {
		t.Fatal(err)
	}
	carol := dial(t, srv, "carol")
	if _, err := carol.Request(ctx(t), map[string]any{"type": "join_room", "room_id": roomID}); err != nil {
		t.Fatal(err)
	}

	before := time.Now().UnixMilli()
	if err := bob.Send(map[string]any{"type": "chat", "text": "hello everyone"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*client.Control{"alice": alice, "bob": bob, "carol": carol} {
		m, err := c.WaitEvent(ctx(t), protocol.TypeChat, nil)
		if err != nil {
			t.Fatalf("%s chat: %v", name, err)
		}
		if m.FromName != "bob" || m.Text != "hello everyone" || m.ServerTimeMS < before || m.ServerTimeMS > time.Now().UnixMilli() {
			t.Fatalf("%s got chat %+v", name, m)
		}
	}

	if _, err := bob.Request(ctx(t), map[string]any{"type": "leave_room"}); err != nil {
		t.Fatal(err)
	}
	var se *client.ServerError
	if _, err := bob.Request(ctx(t), map[string]any{"type": "cancel_room"}); !errors.As(err, &se) || se.Code != protocol.CodeNotInRoom {
		t.Fatalf("cancel outside room: %v", err)
	}
	if _, err := carol.Request(ctx(t), map[string]any{"type": "cancel_room"}); !errors.As(err, &se) || se.Code != protocol.CodeNotHost {
		t.Fatalf("participant cancel: %v", err)
	}
	closed, err := alice.Request(ctx(t), map[string]any{"type": "cancel_room"})
	if err != nil || closed.Type != protocol.TypeRoomClosed || closed.Reason != protocol.ReasonCancelled {
		t.Fatalf("host cancel: %+v %v", closed, err)
	}
	if m, err := carol.WaitEvent(ctx(t), protocol.TypeRoomClosed, nil); err != nil || m.RoomID != roomID {
		t.Fatalf("carol not removed: %+v %v", m, err)
	}
	if list, _ := bob.Request(ctx(t), map[string]any{"type": "list_rooms"}); len(list.Rooms) != 0 {
		t.Fatalf("cancelled room still listed: %+v", list.Rooms)
	}
	if _, err := bob.Request(ctx(t), map[string]any{"type": "join_room", "room_id": roomID}); !errors.As(err, &se) || se.Code != protocol.CodeRoomNotFound {
		t.Fatalf("join cancelled room: %v", err)
	}

	// A room closes on its own when its last member leaves.
	dave := dial(t, srv, "dave")
	if _, err := dave.Request(ctx(t), map[string]any{"type": "create_room"}); err != nil {
		t.Fatal(err)
	}
	_ = dave.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, _ := bob.Request(ctx(t), map[string]any{"type": "list_rooms"})
		if len(list.Rooms) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("room of a disconnected client was not closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestModeSwitchP2PRelayP2P(t *testing.T) {
	srv := startServer(t)
	stats := srv.Router.Stats()
	alice := enter(t, srv, "alice", 0)
	bob := enter(t, srv, "bob", alice.joined.RoomID)

	am, bm := alice.waitMode(t, "p2p"), bob.waitMode(t, "p2p")
	if am.Peer == nil || am.Peer.Addr != bob.media.LocalAddr().String() || bm.Peer.Addr != alice.media.LocalAddr().String() {
		t.Fatalf("peer addresses: %+v / %+v", am.Peer, bm.Peer)
	}
	if am.Peer.VideoSSRC != bob.joined.VideoSSRC {
		t.Fatalf("peer SSRCs: %+v", am.Peer)
	}

	// In P2P mode media goes peer to peer and the server forwards nothing.
	peerAddr := netip.MustParseAddrPort(am.Peer.Addr)
	for i := uint32(0); i < 5; i++ {
		alice.sendTo(peerAddr, alice.video(i))
	}
	if got := bob.recv(200 * time.Millisecond); countFrom(got, alice.joined.VideoSSRC) != 5 || got[0].from != alice.media.LocalAddr() {
		t.Fatalf("bob got %d direct packets", len(got))
	}
	if out := stats.PacketsOut[media.OutVideo].Load() + stats.PacketsOut[media.OutAudio].Load(); out != 0 {
		t.Fatalf("server sent %d media packets while the room was P2P", out)
	}
	// Media sent to the server after the grace period is dropped, not relayed.
	time.Sleep(relayGrace + 50*time.Millisecond)
	alice.sendServer(alice.video(99))
	if got := bob.recv(150 * time.Millisecond); len(got) != 0 {
		t.Fatal("server relayed a packet in P2P mode")
	}
	if stats.Snapshot().Drops["p2p_mode"] == 0 {
		t.Fatal("p2p_mode drop not counted")
	}

	// A third participant switches everyone to the relay.
	carol := enter(t, srv, "carol", alice.joined.RoomID)
	for _, m := range []*member{alice, bob} {
		if msg := m.waitMode(t, "relay"); msg.Peer != nil {
			t.Fatalf("%s: relay mode with a peer: %+v", m.name, msg)
		}
	}
	alice.sendServer(alice.video(100))
	for _, m := range []*member{bob, carol} {
		if got := m.recv(200 * time.Millisecond); countFrom(got, alice.joined.VideoSSRC) != 1 || got[0].from != alice.media.Server {
			t.Fatalf("%s: relayed video missing (%d packets)", m.name, len(got))
		}
	}

	// Mixed audio: alice and bob talk, every member gets everyone else's voice.
	for i := 0; i < 15; i++ {
		ts := uint32(i * protocol.AudioSamplesPerFrame)
		alice.sendServer(alice.audio(ts, 1000))
		bob.sendServer(bob.audio(ts, 2000))
		time.Sleep(20 * time.Millisecond)
	}
	mixer := alice.joined.MixerSSRC
	expect := map[*member]struct {
		value int16
		csrc  []uint32
	}{
		carol: {3000, []uint32{alice.joined.AudioSSRC, bob.joined.AudioSSRC}},
		alice: {2000, []uint32{bob.joined.AudioSSRC}},
		bob:   {1000, []uint32{alice.joined.AudioSSRC}},
	}
	for m, want := range expect {
		good := 0
		for _, p := range m.recv(300 * time.Millisecond) {
			if p.hdr.SSRC != mixer || p.hdr.PayloadType != protocol.PTAudio {
				continue
			}
			v := int16(binary.BigEndian.Uint16(p.body))
			if v == want.value && int(p.hdr.CSRCCount) == len(want.csrc) &&
				p.hdr.CSRC[0] == want.csrc[0] && p.hdr.CSRC[len(want.csrc)-1] == want.csrc[len(want.csrc)-1] {
				good++
			}
		}
		if good < 8 {
			t.Fatalf("%s: %d mixed frames with value %d", m.name, good, want.value)
		}
	}

	// Back to two members: P2P again.
	if _, err := carol.ctl.Request(ctx(t), map[string]any{"type": "leave_room"}); err != nil {
		t.Fatal(err)
	}
	if msg := alice.waitMode(t, "p2p"); msg.Peer.Addr != bob.media.LocalAddr().String() {
		t.Fatalf("p2p peer after switch back: %+v", msg.Peer)
	}
	bob.waitMode(t, "p2p")
	time.Sleep(relayGrace + 50*time.Millisecond)
	forwarded := stats.PacketsOut[media.OutVideo].Load()
	bob.sendTo(alice.media.LocalAddr(), bob.video(7))
	if got := alice.recv(150 * time.Millisecond); countFrom(got, bob.joined.VideoSSRC) != 1 {
		t.Fatal("direct packet after switching back to P2P not received")
	}
	if stats.PacketsOut[media.OutVideo].Load() != forwarded {
		t.Fatal("server forwarded video after switching back to P2P")
	}
}

func TestMeetingIsolationAndSpoofing(t *testing.T) {
	srv := startServer(t)
	a1 := enter(t, srv, "a1", 0)
	a2, a3 := enter(t, srv, "a2", a1.joined.RoomID), enter(t, srv, "a3", a1.joined.RoomID)
	b1 := enter(t, srv, "b1", 0)
	b2, b3 := enter(t, srv, "b2", b1.joined.RoomID), enter(t, srv, "b3", b1.joined.RoomID)
	for _, m := range []*member{a1, a2, a3} {
		m.waitMode(t, "relay")
	}

	// Video and audio from room A reach room A only.
	a1.sendServer(a1.video(1))
	for i := 0; i < 10; i++ {
		a1.sendServer(a1.audio(uint32(i*protocol.AudioSamplesPerFrame), 500))
		time.Sleep(20 * time.Millisecond)
	}
	for _, m := range []*member{a2, a3} {
		got := m.recv(200 * time.Millisecond)
		if countFrom(got, a1.joined.VideoSSRC) != 1 || countFrom(got, a1.joined.MixerSSRC) == 0 {
			t.Fatalf("%s missing room A media", m.name)
		}
	}
	for _, m := range []*member{b1, b2, b3} {
		if got := m.recv(100 * time.Millisecond); len(got) != 0 {
			t.Fatalf("%s received %d packets from another room", m.name, len(got))
		}
	}

	// b1 tries to inject into room A using a1's SSRC, and an outsider spoofs a1.
	dropsBefore := srv.Router.Stats().Snapshot().Drops["addr_mismatch"]
	b1.sendServer(a1.video(2))
	outsider, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer outsider.Close()
	_, _ = outsider.WriteToUDPAddrPort(a1.video(3), a1.media.Server)
	for _, m := range []*member{a2, a3, b2, b3} {
		if got := m.recv(100 * time.Millisecond); len(got) != 0 {
			t.Fatalf("spoofed packet reached %s", m.name)
		}
	}
	if d := srv.Router.Stats().Snapshot().Drops["addr_mismatch"] - dropsBefore; d != 2 {
		t.Fatalf("addr_mismatch drops = %d, want 2", d)
	}
}

func rawConn(t *testing.T, srv *Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", srv.ControlAddr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	return c, bufio.NewReader(c)
}

func writeFrame(t *testing.T, c net.Conn, body string) {
	t.Helper()
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func readMsg(t *testing.T, r *bufio.Reader) client.Message {
	t.Helper()
	body, err := protocol.ReadFrame(r, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m client.Message
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestControlProtocolErrors(t *testing.T) {
	srv := startServer(t)

	c, r := rawConn(t, srv)
	writeFrame(t, c, `{"type":"join_room","room_id":1,"seq":4}`)
	if m := readMsg(t, r); m.Code != protocol.CodeNotHello || m.Seq == nil || *m.Seq != 4 {
		t.Fatalf("before hello: %+v", m)
	}
	writeFrame(t, c, `{"type":"hello","name":"x","version":1}`)
	if m := readMsg(t, r); m.Type != protocol.TypeWelcome {
		t.Fatalf("hello: %+v", m)
	}
	writeFrame(t, c, `{not json`)
	if m := readMsg(t, r); m.Code != protocol.CodeBadRequest {
		t.Fatalf("malformed: %+v", m)
	}
	// A message split across several TCP writes is still parsed as one.
	msg := `{"type":"ping","seq":9}`
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(msg)))
	for _, part := range [][]byte{hdr[:2], hdr[2:], []byte(msg[:5]), []byte(msg[5:])} {
		_, _ = c.Write(part)
		time.Sleep(10 * time.Millisecond)
	}
	if m := readMsg(t, r); m.Type != protocol.TypePong || *m.Seq != 9 {
		t.Fatalf("split frame: %+v", m)
	}
	// An oversized frame gets too_large and the connection is closed.
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, protocol.MaxControlMessage+1)
	_, _ = c.Write(big)
	if m := readMsg(t, r); m.Code != protocol.CodeTooLarge {
		t.Fatalf("oversized: %+v", m)
	}
	if _, err := protocol.ReadFrame(r, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("connection should be closed, got %v", err)
	}

	c2, r2 := rawConn(t, srv)
	writeFrame(t, c2, `{"type":"hello","name":"y","version":2}`)
	if m := readMsg(t, r2); m.Code != protocol.CodeUnsupportedVersion {
		t.Fatalf("version: %+v", m)
	}
	if _, err := protocol.ReadFrame(r2, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("connection should be closed after a version mismatch, got %v", err)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv := startServer(t)
	a := enter(t, srv, "a", 0)
	a.waitMode(t, "relay")
	resp, err := http.Get("http://" + srv.MetricsAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`confserver_rooms{mode="relay"} 1`,
		`confserver_participants 1`,
		`confserver_media_packets_received_total{kind="register"}`,
		`confserver_media_dropped_packets_total{reason="addr_mismatch"} 0`,
		`confserver_forward_latency_seconds_bucket`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if resp2, err := http.Get("http://" + srv.MetricsAddr() + "/debug/pprof/"); err == nil {
		resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK {
			t.Error("pprof must be off unless enabled")
		}
	}
}

func TestShutdownNotifiesMembers(t *testing.T) {
	srv := startServer(t)
	a := enter(t, srv, "a", 0)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	m, err := a.ctl.WaitEvent(ctx(t), protocol.TypeRoomClosed, nil)
	if err != nil || m.Reason != protocol.ReasonServerShutdown {
		t.Fatalf("room_closed on shutdown: %+v %v", m, err)
	}
}
