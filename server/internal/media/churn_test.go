package media

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// TestChurnForwarding measures how control-plane churn delays forwarding. The server
// holds 64 rooms of 16 members (1024, the default -max-clients). The members of a room
// share one UDP socket, as members behind one NAT would, and every member keeps its
// binding alive with a REGISTER every 5 s, as clients do (about 204 keepalives/s in
// total, 3.2/s per socket: below the per-source REGISTER limit, so the server answers
// every one). Member A streams 50 video packets/s to member B of the same room for
// CHURN_SECONDS, and the test records each packet's forwarding latency (A's send to
// B's receipt, both on loopback). Churn comes from "spammers", members of other rooms
// that toggle media_state 50 times a second (the default per-connection request
// budget); from "rebinders", members of other rooms that try every 100 ms to move
// their binding to their other socket and wait (up to 50 ms) for the move to complete;
// and from one "flooder", a member that sends REGISTERs alternately from two sockets
// at 1500/s without waiting (below the server-wide 2000/s REGISTER cap). Rebinders and
// the flooder send no other keepalives.
//
// Every member answers the server's challenges as clients do: a rebinder or the
// flooder echoes the nonce of each challenge it receives. The last configuration
// raises the per-member rebind limit so that every rebind of the rebinders is served.
//
// It is a measurement, not a pass/fail test: it runs only when CONFSERVER_CHURN_OUT
// names a file, and appends one JSON line per configuration to it
// (bench/run_churn.sh runs it and collects the lines).
func TestChurnForwarding(t *testing.T) {
	out := os.Getenv("CONFSERVER_CHURN_OUT")
	if out == "" {
		t.Skip("set CONFSERVER_CHURN_OUT to run the churn measurement")
	}
	seconds := 5
	if v, err := strconv.Atoi(os.Getenv("CHURN_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range []struct {
		name                         string
		spammers, rebinders, floodHz int
		unlimitedRebinds             bool
	}{
		{"keepalives", 0, 0, 0, false},
		{"keepalives+media_state", 20, 0, 0, false},
		{"keepalives+media_state+rebinds", 20, 4, 0, false},
		{"keepalives+register_flood", 0, 0, 1500, false},
		{"keepalives+media_state+rebinds_all_served", 20, 4, 0, true},
	} {
		res := churnRun(t, c.spammers, c.rebinders, c.floodHz, c.unlimitedRebinds, seconds)
		res["config"] = c.name
		line, _ := json.Marshal(res)
		if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s", line)
	}
}

type churnMember struct {
	client *room.Client
	joined protocol.Joined
	token  protocol.Token
}

func churnRun(t *testing.T, spammers, rebinders, floodHz int, unlimitedRebinds bool, seconds int) map[string]any {
	s := newTestServer(t, func(rc *room.Config) { rc.MaxClients = 4096 }, func(mc *Config) {
		if unlimitedRebinds {
			mc.RebindsPerSec, mc.RebindBurst = 1000, 1000
		}
	})
	const rooms, size = 64, 16
	var sinks [rooms]*net.UDPConn
	var sinkAddrs [rooms]netip.AddrPort
	for r := range rooms {
		sinks[r], sinkAddrs[r] = churnSocket(t)
		go churnDrain(sinks[r])
	}
	var all [rooms][size]churnMember
	for r := range rooms {
		var roomID uint32
		for m := range size {
			capt := &joinedCapture{}
			c, err := s.reg.Hello(fmt.Sprintf("u%d-%d", r, m), capt)
			if err != nil {
				t.Fatal(err)
			}
			if m == 0 {
				err = s.reg.CreateRoom(c, "", nil)
			} else {
				err = s.reg.JoinRoom(c, roomID, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			capt.mu.Lock()
			j := capt.joined
			capt.mu.Unlock()
			roomID = j.RoomID
			tok, err := protocol.ParseToken(j.Token)
			if err != nil {
				t.Fatal(err)
			}
			all[r][m] = churnMember{client: c, joined: j, token: tok}
		}
	}
	a, aAddr := churnSocket(t)
	b, bAddr := churnSocket(t)
	type keepalive struct {
		conn *net.UDPConn
		pkt  []byte
	}
	var keep []keepalive
	for r := range rooms {
		for m := range size {
			conn, addr := sinks[r], sinkAddrs[r]
			switch {
			case r == 0 && m == 0:
				conn, addr = a, aAddr
			case r == 0 && m == 1:
				conn, addr = b, bAddr
			}
			churnBind(t, s, all[r][m].token, addr)
			rebinder := m == 1 && r >= 40 && r < 40+rebinders
			flooder := floodHz > 0 && r == 50 && m == 2
			if !rebinder && !flooder {
				keep = append(keep, keepalive{conn, churnRegister(all[r][m].token, 1)})
			}
		}
	}
	s.router.Sync()
	time.Sleep(200 * time.Millisecond)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Spammers: the first member of rooms 10..10+spammers-1.
	for k := range spammers {
		c := all[10+k][0].client
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := time.Now()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.reg.SetMediaState(c, i%2 == 0, true, nil)
				next = next.Add(20 * time.Millisecond)
				time.Sleep(time.Until(next))
			}
		}()
	}
	// Rebinders: the second member of rooms 40..40+rebinders-1. Every 100 ms each tries
	// to move its binding to the socket it is not bound to, waiting (up to 50 ms) for
	// the move to be acknowledged; a move that is not acknowledged is retried to the
	// same socket.
	var rebinds, rebindsDone atomic.Int64
	for k := range rebinders {
		tok := all[40+k][1].token
		socks := [2]*net.UDPConn{}
		socks[0], _ = churnSocket(t)
		socks[1], _ = churnSocket(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := 0
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
				rebinds.Add(1)
				if churnRebind(socks[target], s.addr, tok, uint64(i)) {
					rebindsDone.Add(1)
					target = 1 - target
				}
			}
		}()
	}
	// Flooder: the third member of room 50 sends REGISTERs alternately from two sockets
	// at floodHz, without waiting for replies.
	if floodHz > 0 {
		tok := all[50][2].token
		f1, _ := churnSocket(t)
		f2, _ := churnSocket(t)
		go churnEcho(f1, s.addr, tok)
		go churnEcho(f2, s.addr, tok)
		pkt := churnRegister(tok, 9)
		wg.Add(1)
		go func() {
			defer wg.Done()
			iv := time.Second / time.Duration(floodHz)
			next := time.Now()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				conn := f1
				if i%2 == 1 {
					conn = f2
				}
				_, _ = conn.WriteToUDPAddrPort(pkt, s.addr)
				next = next.Add(iv)
				time.Sleep(time.Until(next))
			}
		}()
	}
	// Keepalives: every other member once per 5 s, spread evenly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		iv := 5 * time.Second / time.Duration(len(keep))
		next := time.Now()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := keep[i%len(keep)]
			_, _ = k.conn.WriteToUDPAddrPort(k.pkt, s.addr)
			next = next.Add(iv)
			time.Sleep(time.Until(next))
		}
	}()

	// The measured stream: A -> B, 50 packets/s.
	pa := &peer{t: t, joined: all[0][0].joined, conn: a}
	total := 50 * seconds
	sendTimes := make([]atomic.Int64, total+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		next := time.Now()
		for i := 1; i <= total; i++ {
			pkt := pa.videoPacket(uint32(i))
			sendTimes[i].Store(time.Now().UnixNano())
			_, _ = a.WriteToUDPAddrPort(pkt, s.addr)
			next = next.Add(20 * time.Millisecond)
			time.Sleep(time.Until(next))
		}
	}()
	var lat []float64
	seen := make(map[uint32]bool)
	buf := make([]byte, 2048)
	deadline := time.Now().Add(time.Duration(seconds)*time.Second + time.Second)
	for {
		_ = b.SetReadDeadline(deadline)
		n, err := b.Read(buf)
		if err != nil {
			break
		}
		var h protocol.RTPHeader
		if _, err := protocol.ParseRTP(buf[:n], &h); err != nil || h.PayloadType != protocol.PTVideo {
			continue
		}
		id := h.Timestamp / 6000
		if id == 0 || int(id) > total || seen[id] {
			continue
		}
		seen[id] = true
		lat = append(lat, float64(time.Now().UnixNano()-sendTimes[id].Load())/1e6)
	}
	close(stop)
	wg.Wait()
	sort.Float64s(lat)
	q := func(p float64) float64 {
		if len(lat) == 0 {
			return -1
		}
		return lat[int(p*float64(len(lat)-1)+0.5)]
	}
	snap := s.router.Stats().Snapshot()
	return map[string]any{
		"members":           rooms * size,
		"spammers":          spammers,
		"rebinders":         rebinders,
		"rebinds":           rebinds.Load(),
		"rebinds_completed": rebindsDone.Load(),
		"flood_per_s":       floodHz,
		"keepalives_per_s":  float64(len(keep)) / 5,
		"seconds":           seconds,
		"sent":              total,
		"delivered":         len(lat),
		"latency_ms": map[string]float64{
			"p50": q(0.5), "p90": q(0.9), "p99": q(0.99), "max": q(1),
		},
		"drops":          snap.Drops,
		"packets_in":     snap.PacketsIn,
		"packets_out":    snap.PacketsOut,
		"bindings":       s.router.Stats().Bindings.Load(),
		"table_rebuilds": s.router.Stats().TableRebuilds.Load(),
		"room_rebuilds":  s.router.Stats().RoomRebuilds.Load(),
	}
}

func churnSocket(t *testing.T) (*net.UDPConn, netip.AddrPort) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func churnDrain(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		if _, err := c.Read(buf); err != nil {
			return
		}
	}
}

func churnRegister(tok protocol.Token, txID uint64) []byte {
	reg := protocol.Register{TxID: txID, Token: tok}
	b := make([]byte, protocol.RegisterSize)
	n, _ := reg.MarshalTo(b)
	return b[:n]
}

// churnRebind moves tok's binding to conn, echoing the server's challenge, and reports
// whether the server acknowledged the move within 50 ms. A plain acknowledgement of
// the first REGISTER also counts: conn is already bound, because an earlier move
// completed after its 50 ms had passed.
func churnRebind(conn *net.UDPConn, server netip.AddrPort, tok protocol.Token, txID uint64) bool {
	reg := protocol.Register{TxID: txID, Token: tok}
	pkt := make([]byte, protocol.RegisterSize)
	buf := make([]byte, 256)
	deadline := time.Now().Add(50 * time.Millisecond)
	for range 3 {
		n, _ := reg.MarshalTo(pkt)
		if _, err := conn.WriteToUDPAddrPort(pkt[:n], server); err != nil {
			return false
		}
		var ack protocol.RegisterAck
		for {
			_ = conn.SetReadDeadline(deadline)
			n, err := conn.Read(buf)
			if err != nil {
				return false
			}
			if protocol.ParseRegisterAck(buf[:n], &ack) == nil && ack.TxID == txID {
				break
			}
		}
		if !ack.Challenge {
			return true
		}
		reg.HasNonce, reg.Nonce = true, ack.Nonce
	}
	return false
}

// churnEcho reads conn until it is closed and answers every challenge for tok by
// echoing its nonce, as a client does.
func churnEcho(conn *net.UDPConn, server netip.AddrPort, tok protocol.Token) {
	buf := make([]byte, 256)
	pkt := make([]byte, protocol.RegisterSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		var ack protocol.RegisterAck
		if protocol.ParseRegisterAck(buf[:n], &ack) != nil || !ack.Challenge {
			continue
		}
		reg := protocol.Register{TxID: ack.TxID, Token: tok, HasNonce: true, Nonce: ack.Nonce}
		m, _ := reg.MarshalTo(pkt)
		_, _ = conn.WriteToUDPAddrPort(pkt[:m], server)
	}
}

// churnBind binds tok to addr through the control plane, as a completed REGISTER would.
func churnBind(t *testing.T, s *testServer, tok protocol.Token, addr netip.AddrPort) {
	t.Helper()
	if !s.reg.Register(tok, addr) {
		t.Fatal("register failed")
	}
}
