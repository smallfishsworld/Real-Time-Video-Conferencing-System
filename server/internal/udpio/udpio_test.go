package udpio

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"
)

// modes runs a test with batching and GSO, batching alone, and neither. On platforms
// without recvmmsg the first two check the fallback instead.
func modes(t *testing.T, f func(t *testing.T, opt Options)) {
	for _, opt := range []Options{{Batching: true, GSO: true}, {Batching: true}, {}} {
		t.Run(fmt.Sprintf("batching=%v,gso=%v", opt.Batching, opt.GSO), func(t *testing.T) { f(t, opt) })
	}
}

func listen(t testing.TB, network, addr string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr)))
	if err != nil {
		t.Skipf("listen %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func addrOf(c *net.UDPConn) netip.AddrPort { return c.LocalAddr().(*net.UDPAddr).AddrPort() }

func slots(n, size int) []Message {
	ms := make([]Message, n)
	for i := range ms {
		ms[i].Buf = make([]byte, size)
	}
	return ms
}

func TestBatchingAvailable(t *testing.T) {
	c := New(listen(t, "udp4", "127.0.0.1:0"), Options{Batching: true, GSO: true})
	want := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if c.Batching() != want {
		t.Fatalf("Batching() = %v on %s/%s", c.Batching(), runtime.GOOS, runtime.GOARCH)
	}
	t.Logf("GSO available: %v", c.GSO())
	if c := New(listen(t, "udp4", "127.0.0.1:0"), Options{GSO: true}); c.Batching() || c.GSO() {
		t.Fatal("batching or GSO enabled although batching is off")
	}
}

func TestReadBatch(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		rx := listen(t, "udp4", "127.0.0.1:0")
		tx := listen(t, "udp4", "127.0.0.1:0")
		c := New(rx, opt)
		const total = 100
		for i := range total {
			if _, err := tx.WriteToUDPAddrPort([]byte(fmt.Sprintf("datagram %03d", i)), addrOf(rx)); err != nil {
				t.Fatal(err)
			}
		}
		ms := slots(64, 2048)
		got, calls, largest := 0, 0, 0
		for got < total {
			_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := c.ReadBatch(ms)
			if err != nil {
				t.Fatalf("after %d datagrams: %v", got, err)
			}
			calls++
			largest = max(largest, n)
			for i := range n {
				if want := fmt.Sprintf("datagram %03d", got); string(ms[i].Buf[:ms[i].N]) != want {
					t.Fatalf("datagram %d = %q, want %q", got, ms[i].Buf[:ms[i].N], want)
				}
				if ms[i].Addr != addrOf(tx) {
					t.Fatalf("source %v, want %v", ms[i].Addr, addrOf(tx))
				}
				got++
			}
		}
		if c.Batching() && largest < 2 {
			t.Fatalf("recvmmsg returned one datagram per call (%d calls)", calls)
		}
		if !c.Batching() && calls != total {
			t.Fatalf("%d calls for %d datagrams without batching", calls, total)
		}
	})
}

// A datagram longer than the buffer is truncated to it on Linux and macOS. Windows
// reports it as an error without its source address, so there ReadBatch drops it and
// returns the next datagram.
func TestReadBatchTruncates(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		rx := listen(t, "udp4", "127.0.0.1:0")
		tx := listen(t, "udp4", "127.0.0.1:0")
		c := New(rx, opt)
		for _, n := range []int{3000, 100} {
			if _, err := tx.WriteToUDPAddrPort(bytes.Repeat([]byte{7}, n), addrOf(rx)); err != nil {
				t.Fatal(err)
			}
		}
		want := []int{2048, 100}
		if runtime.GOOS == "windows" {
			want = want[1:]
		}
		ms := slots(4, 2048)
		_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
		var got []int
		for len(got) < len(want) {
			n, err := c.ReadBatch(ms)
			if err != nil {
				t.Fatalf("ReadBatch after datagrams of %v: %v", got, err)
			}
			for _, m := range ms[:n] {
				if m.Addr != addrOf(tx) {
					t.Fatalf("source %v, want %v", m.Addr, addrOf(tx))
				}
				got = append(got, m.N)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("datagram lengths %v, want %v", got, want)
		}
	})
}

func TestWriterFlush(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		c := New(listen(t, "udp4", "127.0.0.1:0"), opt)
		rxs := []*net.UDPConn{listen(t, "udp4", "127.0.0.1:0"), listen(t, "udp4", "127.0.0.1:0")}
		dsts := []Dest{c.Dest(addrOf(rxs[0])), c.Dest(addrOf(rxs[1]))}
		w := c.NewWriter()
		bufs := make([][]byte, 40)
		for i := range bufs {
			bufs[i] = []byte(fmt.Sprintf("copy %02d", i))
			w.Add(bufs[i], &dsts[i%2], uint8(i%3))
		}
		if w.Len() != len(bufs) {
			t.Fatalf("Len = %d", w.Len())
		}
		res := w.Flush()
		if w.Len() != 0 {
			t.Fatalf("Len after Flush = %d", w.Len())
		}
		var packets, size int
		for tag := range MaxTags {
			packets += res.Packets[tag]
			size += res.Bytes[tag]
		}
		if res.Failed != 0 || packets != len(bufs) || size != 7*len(bufs) || res.Packets[0] != 14 {
			t.Fatalf("Result = %+v", res)
		}
		buf := make([]byte, 64)
		for i := range bufs {
			rx := rxs[i%2]
			_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, src, err := rx.ReadFromUDPAddrPort(buf)
			if err != nil {
				t.Fatal(err)
			}
			if string(buf[:n]) != string(bufs[i]) || src.Port() != addrOf(c.uc).Port() {
				t.Fatalf("receiver %d got %q from %v, want %q", i%2, buf[:n], src, bufs[i])
			}
		}
	})
}

// Runs of equal-length datagrams for one destination, each optionally ended by a
// shorter one, must arrive intact and in order whether or not GSO merges them.
func TestWriterGSORuns(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		c := New(listen(t, "udp4", "127.0.0.1:0"), opt)
		rxA, rxB := listen(t, "udp4", "127.0.0.1:0"), listen(t, "udp4", "127.0.0.1:0")
		for _, rx := range []*net.UDPConn{rxA, rxB} {
			_ = rx.SetReadBuffer(1 << 20)
		}
		a, b := c.Dest(addrOf(rxA)), c.Dest(addrOf(rxB))
		type dgram struct {
			d    *Dest
			size int
		}
		var plan []dgram
		add := func(d *Dest, size, count int) {
			for range count {
				plan = append(plan, dgram{d, size})
			}
		}
		add(&a, 1220, 10) // one message with GSO
		add(&a, 500, 1)   // shorter: ends it
		add(&b, 1232, 3)  // another destination
		add(&a, 1220, 2)  // a again: a new message
		add(&a, 1232, 1)  // longer: cannot join, starts another
		add(&b, 100, 70)  // more than maxSegments: split
		w := c.NewWriter()
		sent := make([][]byte, len(plan))
		for i, p := range plan {
			sent[i] = bytes.Repeat([]byte{byte(i)}, p.size)
			w.Add(sent[i], p.d, 0)
		}
		res := w.Flush()
		if res.Failed != 0 || res.Packets[0] != len(plan) {
			t.Fatalf("Result = %+v", res)
		}
		switch {
		case c.GSO():
			// a:10+1, b:3, a:2, a:1, b:64, b:6
			if res.Messages != 6 {
				t.Fatalf("%d messages with GSO, want 6", res.Messages)
			}
		case res.Messages != len(plan):
			t.Fatalf("%d messages for %d datagrams without GSO", res.Messages, len(plan))
		}
		buf := make([]byte, 2048)
		for _, rx := range []*net.UDPConn{rxA, rxB} {
			for i, p := range plan {
				if p.d.AddrPort() != addrOf(rx) {
					continue
				}
				_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, _, err := rx.ReadFromUDPAddrPort(buf)
				if err != nil {
					t.Fatalf("datagram %d: %v", i, err)
				}
				if !bytes.Equal(buf[:n], sent[i]) {
					t.Fatalf("datagram %d: got %d bytes starting %d, want %d bytes of %d", i, n, buf[0], p.size, byte(i))
				}
			}
		}
	})
}

func TestWriterSkipsRefusedDatagrams(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		c := New(listen(t, "udp4", "127.0.0.1:0"), opt)
		rx := listen(t, "udp4", "127.0.0.1:0")
		good := c.Dest(addrOf(rx))
		bad := c.Dest(netip.MustParseAddrPort("[2001:db8::1]:9")) // not reachable from an IPv4 socket
		w := c.NewWriter()
		w.Add([]byte("a"), &good, 0)
		w.Add([]byte("b"), &bad, 0)
		w.Add([]byte("c"), &good, 0)
		if res := w.Flush(); res.Failed != 1 || res.Packets[0] != 2 || res.Bytes[0] != 2 {
			t.Fatalf("Result = %+v, want 2 sent and 1 failed", res)
		}
		buf := make([]byte, 8)
		for _, want := range []string{"a", "c"} {
			_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := rx.ReadFromUDPAddrPort(buf)
			if err != nil || string(buf[:n]) != want {
				t.Fatalf("got %q, %v; want %q", buf[:n], err, want)
			}
		}
	})
}

// A dual-stack socket sees IPv4 peers as IPv4-mapped addresses; ReadBatch reports them
// unmapped, and a Dest made from that address must reach the peer again.
func TestDualStackSocket(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		server := listen(t, "udp", "[::]:0")
		peer := listen(t, "udp4", "127.0.0.1:0")
		c := New(server, opt)
		to := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), addrOf(server).Port())
		if _, err := peer.WriteToUDPAddrPort([]byte("ping"), to); err != nil {
			t.Fatal(err)
		}
		ms := slots(8, 2048)
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.ReadBatch(ms)
		if err != nil || n != 1 {
			t.Fatalf("ReadBatch = %d, %v", n, err)
		}
		if ms[0].Addr != addrOf(peer) {
			t.Fatalf("source %v, want %v", ms[0].Addr, addrOf(peer))
		}
		d := c.Dest(ms[0].Addr)
		w := c.NewWriter()
		w.Add([]byte("pong"), &d, 1)
		if res := w.Flush(); res.Packets[1] != 1 {
			t.Fatalf("Result = %+v", res)
		}
		buf := make([]byte, 8)
		_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, _, err := peer.ReadFromUDPAddrPort(buf); err != nil || string(buf[:n]) != "pong" {
			t.Fatalf("peer got %q, %v", buf[:n], err)
		}
	})
}

func TestIPv6(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		a := listen(t, "udp6", "[::1]:0")
		b := listen(t, "udp6", "[::1]:0")
		c := New(a, opt)
		d := c.Dest(addrOf(b))
		w := c.NewWriter()
		w.Add([]byte("six"), &d, 0)
		if res := w.Flush(); res.Packets[0] != 1 {
			t.Fatalf("Result = %+v", res)
		}
		buf := make([]byte, 8)
		_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, src, err := b.ReadFromUDPAddrPort(buf)
		if err != nil || string(buf[:n]) != "six" || src != addrOf(a) {
			t.Fatalf("got %q from %v, %v", buf[:n], src, err)
		}
		if _, err := b.WriteToUDPAddrPort([]byte("back"), addrOf(a)); err != nil {
			t.Fatal(err)
		}
		ms := slots(2, 64)
		_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := c.ReadBatch(ms); err != nil || n != 1 || ms[0].Addr != addrOf(b) {
			t.Fatalf("ReadBatch = %d, %v, from %v", n, err, ms[0].Addr)
		}
	})
}

func TestReadBatchAfterClose(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		rx := listen(t, "udp4", "127.0.0.1:0")
		c := New(rx, opt)
		done := make(chan error, 1)
		go func() {
			_, err := c.ReadBatch(slots(4, 64))
			done <- err
		}()
		time.Sleep(20 * time.Millisecond)
		_ = rx.Close()
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("ReadBatch after Close: %v, want net.ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ReadBatch did not return after Close")
		}
		w := c.NewWriter()
		d := c.Dest(netip.MustParseAddrPort("127.0.0.1:9"))
		w.Add([]byte("x"), &d, 0)
		if res := w.Flush(); res.Failed != 1 {
			t.Fatalf("Flush on a closed socket: %+v", res)
		}
	})
}

// The hot path must not allocate: reading a batch and flushing queued copies.
func TestNoAllocations(t *testing.T) {
	modes(t, func(t *testing.T, opt Options) {
		rx := listen(t, "udp4", "127.0.0.1:0")
		c := New(rx, opt)
		sink := listen(t, "udp4", "127.0.0.1:0")
		dsts := []Dest{c.Dest(addrOf(sink)), c.Dest(addrOf(sink))}
		w := c.NewWriter()
		ms := slots(16, 2048)
		pkt := []byte("payload")
		round := func() {
			for range 4 {
				if _, err := sink.WriteToUDPAddrPort(pkt, addrOf(rx)); err != nil {
					t.Fatal(err)
				}
			}
			for got := 0; got < 4; {
				n, err := c.ReadBatch(ms)
				if err != nil {
					t.Fatal(err)
				}
				for i := range n {
					w.Add(ms[i].Buf[:ms[i].N], &dsts[i%2], 0)
				}
				got += n
			}
			w.Flush()
		}
		round()
		// Drain the copies the sink receives so its buffer never fills.
		go func() {
			buf := make([]byte, 64)
			for {
				if _, _, err := sink.ReadFromUDPAddrPort(buf); err != nil {
					return
				}
			}
		}()
		if allocs := testing.AllocsPerRun(200, round); allocs != 0 {
			t.Fatalf("%v allocations per round, want 0", allocs)
		}
	})
}
