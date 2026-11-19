package signaling

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

func startSignaling(t *testing.T, mutate func(*Config)) (*Server, string) {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	s := New(room.NewRegistry(room.DefaultConfig()), cfg, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s, ln.Addr().String()
}

func frame(body string) []byte {
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	return out
}

type reply struct {
	Type string `json:"type"`
	Code string `json:"code"`
}

func readReply(t *testing.T, r *bufio.Reader) (reply, error) {
	t.Helper()
	body, err := protocol.ReadFrame(r, nil)
	if err != nil {
		return reply{}, err
	}
	var m reply
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m, nil
}

func dialRaw(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	return c, bufio.NewReader(c)
}

func TestRequestRateLimit(t *testing.T) {
	_, addr := startSignaling(t, func(c *Config) { c.RequestsPerSec = 0.001; c.RequestBurst = 3 })
	c, r := dialRaw(t, addr)
	// hello spends one token; two pings fit, the next three are rejected.
	_, _ = c.Write(frame(`{"type":"hello","name":"n","version":1}`))
	for i := 0; i < 5; i++ {
		_, _ = c.Write(frame(`{"type":"ping"}`))
	}
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		m, err := readReply(t, r)
		if err != nil {
			t.Fatal(err)
		}
		counts[m.Type+"/"+m.Code]++
	}
	if counts["welcome/"] != 1 || counts["pong/"] != 2 || counts["error/rate_limited"] != 3 {
		t.Fatalf("replies %v", counts)
	}
}

func TestHelloTimeoutClosesConnection(t *testing.T) {
	_, addr := startSignaling(t, func(c *Config) { c.HelloTimeout = 100 * time.Millisecond })
	_, r := dialRaw(t, addr)
	start := time.Now()
	if _, err := readReply(t, r); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > 2*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

func TestMaxConns(t *testing.T) {
	s, addr := startSignaling(t, func(c *Config) { c.MaxConns = 1 })
	c1, r1 := dialRaw(t, addr)
	_, _ = c1.Write(frame(`{"type":"hello","name":"first","version":1}`))
	if m, err := readReply(t, r1); err != nil || m.Type != protocol.TypeWelcome {
		t.Fatalf("first connection: %+v %v", m, err)
	}
	_, r2 := dialRaw(t, addr)
	if m, err := readReply(t, r2); err != nil || m.Code != protocol.CodeLimitReached {
		t.Fatalf("second connection: %+v %v", m, err)
	}
	if s.ActiveConns() != 1 || s.TotalConns() != 1 {
		t.Fatalf("active %d total %d", s.ActiveConns(), s.TotalConns())
	}
}

func TestSlowConsumerIsDisconnected(t *testing.T) {
	s := New(room.NewRegistry(room.DefaultConfig()), Config{
		HelloTimeout: time.Second, IdleTimeout: time.Second, WriteTimeout: time.Second,
		OutboxSize: 2, MaxConns: 10,
	}, nil)
	serverSide, clientSide := net.Pipe() // writes block until the peer reads
	defer clientSide.Close()
	s.accept(serverSide)
	var c *conn
	s.mu.Lock()
	for x := range s.conns {
		c = x
	}
	s.mu.Unlock()
	// The writer goroutine takes the first frame and blocks flushing it into the pipe;
	// then two more frames fill the outbox and the next one overflows it.
	msg := []byte{0, 0, 0, 2, '{', '}'}
	c.Send(msg)
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 3; i++ {
		c.Send(msg)
	}
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("slow connection not closed")
	}
	if s.SlowDisconnects() != 1 {
		t.Fatalf("slow disconnects = %d", s.SlowDisconnects())
	}
	// The client side sees the connection end.
	_ = clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := clientSide.Read(buf); err != nil {
			break
		}
	}
}

func TestShutdownRejectsNewConnections(t *testing.T) {
	s, addr := startSignaling(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection served after shutdown")
		}
		c.Close()
	}
}
