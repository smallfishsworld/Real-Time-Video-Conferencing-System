package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/app"
)

func TestHelpIsNotAnError(t *testing.T) {
	if err := run([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v, want flag.ErrHelp (exit 0)", err)
	}
}

func TestInvalidFlagsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"-log-format", "xml"},
		{"-max-ingress-kbps", "-1"},
		{"-ingress-burst-kib", "0"},
		{"-max-rtx-kbps", "0"},
		{"-audio-fec-k", "49"},
		{"-audio-fec-k", "-1"},
		{"-max-room-size", "129"},
		{"-max-room-size", "1"},
		{"-binding-timeout", "1s"},
		{"-udp-batch", "0"},
		{"-control-rate", "0"},
		{"-control-rate", "NaN"},
		{"-max-conns", "-1"},
		{"-max-registers-per-sec", "-1"},
		{"-mixer-delay-frames", "0"},
		{"-mixer-delay-frames", "12"}, // the buffer holds at most 11 frames of delay
		{"stray"},
	} {
		err := run(args)
		if err == nil || errors.Is(err, flag.ErrHelp) || strings.Contains(err.Error(), "listen") {
			t.Errorf("%v: got %v, want a validation error", args, err)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ControlAddr, cfg.MediaAddr, cfg.MetricsAddr = "127.0.0.1:0", "127.0.0.1:0", ""
	srv, err := app.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if err := run([]string{"-healthcheck", srv.ControlAddr()}); err != nil {
		t.Fatalf("healthcheck of a running server: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // accepts, but never says welcome
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	start := time.Now()
	if err := checkHealth(ln.Addr().String(), 300*time.Millisecond); err == nil {
		t.Fatal("healthcheck passed against something that is not a server")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("healthcheck did not time out")
	}
}
