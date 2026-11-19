package signaling

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

func startWith(t *testing.T, rc room.Config, mutate func(*Config), ln net.Listener) (*room.Registry, string) {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	reg := room.NewRegistry(rc)
	s := New(reg, cfg, nil)
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return reg, ln.Addr().String()
}

// readMsg returns the next message as a generic map.
func readMsg(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	body, err := protocol.ReadFrame(r, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMediaStateAndChatRepliesCarrySeq(t *testing.T) {
	_, addr := startWith(t, room.DefaultConfig(), nil, nil)
	c, r := dialRaw(t, addr)
	_, _ = c.Write(frame(`{"type":"hello","name":"ann","version":1,"seq":1}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeWelcome {
		t.Fatalf("hello: %v", m)
	}
	_, _ = c.Write(frame(`{"type":"create_room","seq":2}`))
	for _, want := range []string{protocol.TypeJoined, protocol.TypeRoster, protocol.TypeMode} {
		if m := readMsg(t, r); m["type"] != want {
			t.Fatalf("after create_room got %v, want %s", m, want)
		}
	}
	_, _ = c.Write(frame(`{"type":"media_state","video":false,"audio":true,"seq":3}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeRoster || m["seq"] != float64(3) {
		t.Fatalf("media_state reply %v, want a roster with seq 3", m)
	}
	_, _ = c.Write(frame(`{"type":"chat","text":"hi","seq":4}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeChat || m["seq"] != float64(4) || m["text"] != "hi" {
		t.Fatalf("chat reply %v, want the chat with seq 4", m)
	}
	_, _ = c.Write(frame(`{"type":"chat","text":"   ","seq":5}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeError || m["seq"] != float64(5) || m["code"] != protocol.CodeBadRequest {
		t.Fatalf("rejected chat reply %v, want an error with seq 5", m)
	}
}

func TestHelloDeadlineIsNotExtendedByOtherFrames(t *testing.T) {
	_, addr := startWith(t, room.DefaultConfig(), func(c *Config) { c.HelloTimeout = 300 * time.Millisecond }, nil)
	c, r := dialRaw(t, addr)
	start := time.Now()
	go func() {
		for range 20 { // a ping every 50 ms for 1 s, never a hello
			if _, err := c.Write(frame(`{"type":"ping"}`)); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	for {
		if _, err := protocol.ReadFrame(r, nil); err != nil {
			break
		}
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("connection without hello lived %v (hello timeout 300 ms)", d)
	}
}

func TestMalformedFramesCloseTheConnection(t *testing.T) {
	_, addr := startWith(t, room.DefaultConfig(), nil, nil)
	c, r := dialRaw(t, addr)
	_, _ = c.Write(frame(`{"type":"hello","name":"x","version":1}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeWelcome {
		t.Fatalf("hello: %v", m)
	}
	for range maxMalformed {
		_, _ = c.Write(frame(`{"type":`))
	}
	for range maxMalformed {
		if m := readMsg(t, r); m["code"] != protocol.CodeBadRequest {
			t.Fatalf("got %v", m)
		}
	}
	if _, err := protocol.ReadFrame(r, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("connection still open after %d malformed frames: %v", maxMalformed, err)
	}
}

func TestHelloRefusedWhenFullClosesTheConnection(t *testing.T) {
	rc := room.DefaultConfig()
	rc.MaxClients = 1
	_, addr := startWith(t, rc, nil, nil)
	c1, r1 := dialRaw(t, addr)
	_, _ = c1.Write(frame(`{"type":"hello","name":"a","version":1}`))
	if m := readMsg(t, r1); m["type"] != protocol.TypeWelcome {
		t.Fatalf("first hello: %v", m)
	}
	c2, r2 := dialRaw(t, addr)
	_, _ = c2.Write(frame(`{"type":"hello","name":"b","version":1,"seq":9}`))
	if m := readMsg(t, r2); m["code"] != protocol.CodeLimitReached || m["seq"] != float64(9) {
		t.Fatalf("second hello: %v", m)
	}
	if _, err := protocol.ReadFrame(r2, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("refused connection still open: %v", err)
	}
}

type nopNotifier struct{}

func (nopNotifier) Send([]byte) {}

// The longest titles and names, in 4-byte UTF-8 characters, for every room the server
// allows: the full list would not fit one control message, so it is cut, and says so.
func TestRoomListIsCutToFitOneMessage(t *testing.T) {
	rc := room.DefaultConfig()
	reg, addr := startWith(t, rc, nil, nil)
	wide := "\U0001D11E" // 4 bytes in UTF-8
	for i := range rc.MaxRooms {
		cl, err := reg.Hello(strings.Repeat(wide, room.MaxNameLen-2)+string(rune('a'+i%26))+string(rune('a'+i/26)), nopNotifier{})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.CreateRoom(cl, strings.Repeat(wide, room.MaxTitleLen), nil); err != nil {
			t.Fatal(err)
		}
	}
	c, r := dialRaw(t, addr)
	_, _ = c.Write(frame(`{"type":"hello","name":"x","version":1}`))
	readMsg(t, r)
	_, _ = c.Write(frame(`{"type":"list_rooms","seq":2}`))
	m := readMsg(t, r)
	rooms, _ := m["rooms"].([]any)
	if m["type"] != protocol.TypeRoomList || m["seq"] != float64(2) || m["truncated"] != true ||
		m["total"] != float64(rc.MaxRooms) || len(rooms) == 0 || len(rooms) >= rc.MaxRooms {
		t.Fatalf("room_list: type %v seq %v truncated %v total %v, %d rooms", m["type"], m["seq"], m["truncated"], m["total"], len(rooms))
	}
}

// failingListener returns EMFILE from its first Accept calls, like a process that is
// out of file descriptors for a moment.
type failingListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

func TestAcceptRetriesTemporaryErrors(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &failingListener{Listener: inner}
	ln.failures.Store(3)
	_, addr := startWith(t, room.DefaultConfig(), nil, ln)
	c, r := dialRaw(t, addr)
	_, _ = c.Write(frame(`{"type":"hello","name":"x","version":1}`))
	if m := readMsg(t, r); m["type"] != protocol.TypeWelcome {
		t.Fatalf("after accept errors: %v", m)
	}
}
