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
// holds 64 rooms of 16 members (1024, the default -max-clients). Every member except
// the measured pair keeps its binding alive with a REGISTER every 5 s, as clients do
// (about 205 keepalives/s in total). Member A streams 50 video packets/s to member B
// of the same room for CHURN_SECONDS, and the test records each packet's forwarding
// latency (A's send to B's receipt, both on loopback). Churn comes from "spammers",
// members of other rooms that toggle media_state 50 times a second (the default
// per-connection request budget); from "rebinders", members of other rooms that move
// their binding between two sockets every 100 ms and wait for each move to complete;
// and from one "flooder", a member that sends REGISTERs alternately from two sockets
// at 1500/s without waiting (below the server-wide 2000/s REGISTER cap).
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
	}{
		{"keepalives", 0, 0, 0},
		{"keepalives+media_state", 20, 0, 0},
		{"keepalives+media_state+rebinds", 20, 4, 0},
		{"keepalives+register_flood", 0, 0, 1500},
	} {
		res := churnRun(t, c.spammers, c.rebinders, c.floodHz, seconds)
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

func churnRun(t *testing.T, spammers, rebinders, floodHz, seconds int) map[string]any {
	s := newTestServer(t, func(rc *room.Config) { rc.MaxClients = 4096 }, nil)
	sink, sinkAddr := churnSocket(t)
	go churnDrain(sink)
	const rooms, size = 64, 16
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
	var keep [][]byte
	for r := range rooms {
		for m := range size {
			addr := sinkAddr
			switch {
			case r == 0 && m == 0:
				addr = aAddr
			case r == 0 && m == 1:
				addr = bAddr
			}
			churnBind(t, s, all[r][m].token, addr)
			if addr == sinkAddr {
				keep = append(keep, churnRegister(all[r][m].token, 1))
			}
		}
	}
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
				_ = s.reg.SetMediaState(c, i%2 == 0, true)
				next = next.Add(20 * time.Millisecond)
				time.Sleep(time.Until(next))
			}
		}()
	}
	// Rebinders: the second member of rooms 40..40+rebinders-1, moving between two
	// sockets every 100 ms and waiting (up to 50 ms) for each move to be acknowledged.
	var rebinds, rebindsAcked atomic.Int64
	for k := range rebinders {
		tok := all[40+k][1].token
		socks := [2]*net.UDPConn{}
		socks[0], _ = churnSocket(t)
		socks[1], _ = churnSocket(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
				rebinds.Add(1)
				if churnRebind(socks[i%2], s.addr, tok, uint64(i)) {
					rebindsAcked.Add(1)
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
		go churnDrain(f1)
		go churnDrain(f2)
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
	// Keepalives: every sink-bound member once per 5 s, spread evenly.
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
			_, _ = sink.WriteToUDPAddrPort(keep[i%len(keep)], s.addr)
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
		"members":          rooms * size,
		"spammers":         spammers,
		"rebinders":        rebinders,
		"rebinds":          rebinds.Load(),
		"rebinds_acked":    rebindsAcked.Load(),
		"flood_per_s":      floodHz,
		"keepalives_per_s": float64(len(keep)) / 5,
		"seconds":          seconds,
		"sent":             total,
		"delivered":        len(lat),
		"latency_ms": map[string]float64{
			"p50": q(0.5), "p90": q(0.9), "p99": q(0.99), "max": q(1),
		},
		"drops": snap.Drops,
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

// churnRebind moves tok's binding to conn and reports whether the server acknowledged
// the move within 50 ms.
func churnRebind(conn *net.UDPConn, server netip.AddrPort, tok protocol.Token, txID uint64) bool {
	if _, err := conn.WriteToUDPAddrPort(churnRegister(tok, txID), server); err != nil {
		return false
	}
	buf := make([]byte, 256)
	deadline := time.Now().Add(50 * time.Millisecond)
	for {
		_ = conn.SetReadDeadline(deadline)
		n, err := conn.Read(buf)
		if err != nil {
			return false
		}
		var ack protocol.RegisterAck
		if protocol.ParseRegisterAck(buf[:n], &ack) == nil && ack.TxID == txID {
			return true
		}
	}
}

// churnBind binds tok to addr through the control plane, as a completed REGISTER would.
func churnBind(t *testing.T, s *testServer, tok protocol.Token, addr netip.AddrPort) {
	t.Helper()
	if !s.reg.Register(tok, addr) {
		t.Fatal("register failed")
	}
}
