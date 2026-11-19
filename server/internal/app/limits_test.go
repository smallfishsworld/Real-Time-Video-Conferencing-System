package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// -max-clients above the old fixed 1024-connection limit admits that many clients:
// the control server's connection limit follows it.
func TestMaxClientsAboveOldConnectionLimit(t *testing.T) {
	const clients = 1100
	cfg := DefaultConfig()
	cfg.ControlAddr, cfg.MediaAddr, cfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	cfg.Room.MaxClients = clients
	srv, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	})
	if got := srv.Config().Signaling.MaxConns; got != clients+ConnHeadroom {
		t.Fatalf("MaxConns = %d, want %d", got, clients+ConnHeadroom)
	}
	conns := make([]net.Conn, 0, clients)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range clients {
		c, err := net.Dial("tcp", srv.ControlAddr())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		body := fmt.Sprintf(`{"type":"hello","name":"c%d","version":1}`, i)
		frame := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
		if _, err := c.Write(append(frame, body...)); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		body, err := protocol.ReadFrame(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if msg, err := protocol.DecodeRequest(body); err != nil || msg.Type != protocol.TypeWelcome {
			t.Fatalf("client %d got %s", i, body)
		}
	}
}
