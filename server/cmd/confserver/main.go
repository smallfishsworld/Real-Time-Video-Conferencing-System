// Command confserver runs the conference server: a TCP control plane and one UDP
// media port shared by all rooms. By default every socket binds to loopback; pass
// -control-addr/-media-addr with 0.0.0.0 (or a LAN address) to serve other machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/app"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "confserver:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := app.DefaultConfig()
	fs := flag.NewFlagSet("confserver", flag.ContinueOnError)
	fs.StringVar(&cfg.ControlAddr, "control-addr", cfg.ControlAddr, "TCP control listen address")
	fs.StringVar(&cfg.MediaAddr, "media-addr", cfg.MediaAddr, "UDP media listen address")
	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", cfg.MetricsAddr, "HTTP address for /metrics (empty disables)")
	fs.BoolVar(&cfg.EnablePprof, "pprof", false, "serve /debug/pprof/ on the metrics address")
	fs.IntVar(&cfg.SocketBuffer, "socket-buffer", cfg.SocketBuffer, "media socket send/receive buffer in bytes")
	fs.BoolVar(&cfg.Room.P2PEnabled, "p2p", cfg.Room.P2PEnabled, "let two-member rooms use direct peer-to-peer media")
	fs.IntVar(&cfg.Room.MaxRoomSize, "max-room-size", cfg.Room.MaxRoomSize, "members per room")
	fs.IntVar(&cfg.Room.MaxRooms, "max-rooms", cfg.Room.MaxRooms, "rooms per server")
	fs.IntVar(&cfg.Room.MaxClients, "max-clients", cfg.Room.MaxClients, "connected clients per server")
	fs.DurationVar(&cfg.Room.RelayGrace, "relay-grace", cfg.Room.RelayGrace, "keep relaying this long after a room switches to P2P")
	ingressKbps := fs.Int("max-ingress-kbps", cfg.Media.IngressBytesPerSec*8/1000, "per-sender media rate limit in kbit/s (0 = unlimited)")
	burstKiB := fs.Int("ingress-burst-kib", cfg.Media.IngressBurstBytes>>10, "per-sender burst allowance in KiB")
	fs.IntVar(&cfg.Media.MixerDelayFrames, "mixer-delay-frames", cfg.Media.MixerDelayFrames, "per-sender jitter buffer before the mixer, in 20 ms frames")
	fs.IntVar(&cfg.Media.AudioFECK, "audio-fec-k", cfg.Media.AudioFECK, "XOR parity group size for mixed audio (0 disables FEC)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Media.IngressBytesPerSec = *ingressKbps * 1000 / 8
	cfg.Media.IngressBurstBytes = *burstKiB << 10

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: level}
	if *logFormat == "json" {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	} else {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, opts))
	}

	srv, err := app.Start(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	cfg.Logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
