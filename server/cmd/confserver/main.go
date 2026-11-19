// Command confserver runs the conference server: a TCP control plane and one UDP
// media port shared by all rooms. By default every socket binds to loopback; pass
// -control-addr/-media-addr with 0.0.0.0 (or a LAN address) to serve other machines.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/app"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/media"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	default:
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
	fs.IntVar(&cfg.Room.MaxRoomSize, "max-room-size", cfg.Room.MaxRoomSize, fmt.Sprintf("members per room (at most %d)", room.MaxRoomSizeLimit))
	fs.IntVar(&cfg.Room.MaxRooms, "max-rooms", cfg.Room.MaxRooms, "rooms per server")
	fs.IntVar(&cfg.Room.MaxClients, "max-clients", cfg.Room.MaxClients, "connected clients per server")
	fs.DurationVar(&cfg.Room.RelayGrace, "relay-grace", cfg.Room.RelayGrace, "keep relaying this long after a room switches to P2P")
	ingressKbps := fs.Int("max-ingress-kbps", cfg.Media.IngressBytesPerSec*8/1000, "per-client media and NACK rate limit in kbit/s, IP/UDP headers included (0 = unlimited)")
	burstKiB := fs.Int("ingress-burst-kib", cfg.Media.IngressBurstBytes>>10, "per-client burst allowance in KiB")
	rtxKbps := fs.Int("max-rtx-kbps", cfg.Media.RTXBytesPerSec*8/1000, "retransmissions one client may request, in kbit/s (must be > 0 when -nack-buffer > 0)")
	fs.IntVar(&cfg.Media.RegistersPerSec, "max-registers-per-sec", cfg.Media.RegistersPerSec, "REGISTER packets with a valid token per second, all clients together (0 = no server-wide limit; per-member and per-source limits still apply)")
	fs.DurationVar(&cfg.Media.BindingTimeout, "binding-timeout", cfg.Media.BindingTimeout, "drop a media binding after this long without a keepalive (0 = never)")
	fs.IntVar(&cfg.Media.MixerDelayFrames, "mixer-delay-frames", cfg.Media.MixerDelayFrames, fmt.Sprintf("per-sender jitter buffer before the mixer, in 20 ms frames (1..%d)", media.MaxMixerDelayFrames))
	fs.IntVar(&cfg.Media.AudioFECK, "audio-fec-k", cfg.Media.AudioFECK, fmt.Sprintf("XOR parity group size for mixed audio, 0 to %d (0 disables FEC)", protocol.MaxFECGroup))
	fs.IntVar(&cfg.Media.UDPBatch, "udp-batch", cfg.Media.UDPBatch, "most datagrams per recvmmsg/sendmmsg call (1 = one system call per datagram)")
	fs.BoolVar(&cfg.Media.UDPGSO, "udp-gso", cfg.Media.UDPGSO, "send forwarded video as UDP GSO messages where the kernel supports it")
	fs.IntVar(&cfg.Media.NACKBufferPackets, "nack-buffer", cfg.Media.NACKBufferPackets, "recent video packets kept per sender for NACK retransmission (0 disables)")
	fs.IntVar(&cfg.Signaling.MaxConns, "max-conns", 0, fmt.Sprintf("open control connections (0 = -max-clients + %d)", app.ConnHeadroom))
	fs.Float64Var(&cfg.Signaling.RequestsPerSec, "control-rate", cfg.Signaling.RequestsPerSec, "control frames per second per connection")
	fs.IntVar(&cfg.Signaling.RequestBurst, "control-burst", cfg.Signaling.RequestBurst, "control frames a connection may send in a burst")
	fs.DurationVar(&cfg.Signaling.HelloTimeout, "hello-timeout", cfg.Signaling.HelloTimeout, "time from connect to a successful hello")
	fs.DurationVar(&cfg.Signaling.IdleTimeout, "idle-timeout", cfg.Signaling.IdleTimeout, "close a control connection after this long without a message")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if err := validate(&cfg, *ingressKbps, *burstKiB, *rtxKbps, *logFormat); err != nil {
		return err
	}
	cfg.Media.IngressBytesPerSec = *ingressKbps * 1000 / 8
	cfg.Media.IngressBurstBytes = *burstKiB << 10
	cfg.Media.RTXBytesPerSec = *rtxKbps * 1000 / 8

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
	cfg.Room.Logger = cfg.Logger

	srv, err := app.Start(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var serveErr error
	select {
	case <-ctx.Done():
		cfg.Logger.Info("shutting down")
	case <-srv.Done():
		// A serving goroutine failed: do not keep running half a server.
		serveErr = srv.Err()
		cfg.Logger.Error("shutting down after a serving error", "err", serveErr)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if serveErr != nil {
		return serveErr
	}
	return err
}

// validate rejects values that would leave the server misconfigured.
func validate(cfg *app.Config, ingressKbps, burstKiB, rtxKbps int, logFormat string) error {
	switch {
	case logFormat != "text" && logFormat != "json":
		return fmt.Errorf("-log-format %q: want text or json", logFormat)
	case ingressKbps < 0:
		return fmt.Errorf("-max-ingress-kbps %d: must be 0 (unlimited) or positive", ingressKbps)
	case ingressKbps > 0 && burstKiB < 1:
		return fmt.Errorf("-ingress-burst-kib %d: must be at least 1 when the ingress limit is on", burstKiB)
	case rtxKbps < 0, cfg.Media.NACKBufferPackets > 0 && rtxKbps == 0:
		return fmt.Errorf("-max-rtx-kbps %d: must be positive while -nack-buffer is on", rtxKbps)
	case cfg.Media.NACKBufferPackets < 0:
		return fmt.Errorf("-nack-buffer %d: must be 0 or positive", cfg.Media.NACKBufferPackets)
	case cfg.Media.AudioFECK < 0 || cfg.Media.AudioFECK > protocol.MaxFECGroup:
		return fmt.Errorf("-audio-fec-k %d: must be 0 to %d", cfg.Media.AudioFECK, protocol.MaxFECGroup)
	case cfg.Media.MixerDelayFrames < 1 || cfg.Media.MixerDelayFrames > media.MaxMixerDelayFrames:
		// The buffer would silently clamp anything else.
		return fmt.Errorf("-mixer-delay-frames %d: must be 1 to %d", cfg.Media.MixerDelayFrames, media.MaxMixerDelayFrames)
	case cfg.Media.RegistersPerSec < 0:
		return fmt.Errorf("-max-registers-per-sec %d: must be 0 or positive", cfg.Media.RegistersPerSec)
	case cfg.Media.BindingTimeout < 0 || cfg.Media.BindingTimeout > 0 && cfg.Media.BindingTimeout < 6*time.Second:
		return fmt.Errorf("-binding-timeout %v: must be 0 or at least 6s (clients send a keepalive every 5 s)", cfg.Media.BindingTimeout)
	case cfg.Media.UDPBatch < 1 || cfg.Media.UDPBatch > 1024:
		return fmt.Errorf("-udp-batch %d: must be 1 to 1024", cfg.Media.UDPBatch)
	case cfg.Room.MaxRoomSize < 2 || cfg.Room.MaxRoomSize > room.MaxRoomSizeLimit:
		return fmt.Errorf("-max-room-size %d: must be 2 to %d", cfg.Room.MaxRoomSize, room.MaxRoomSizeLimit)
	case cfg.Room.MaxRooms < 1 || cfg.Room.MaxClients < 1:
		return fmt.Errorf("-max-rooms and -max-clients must be positive")
	case cfg.Signaling.MaxConns < 0:
		return fmt.Errorf("-max-conns %d: must be 0 (derived) or positive", cfg.Signaling.MaxConns)
	case !(cfg.Signaling.RequestsPerSec > 0) || cfg.Signaling.RequestBurst < 1:
		return fmt.Errorf("-control-rate and -control-burst must be positive")
	case cfg.Signaling.HelloTimeout <= 0 || cfg.Signaling.IdleTimeout <= 0:
		return fmt.Errorf("-hello-timeout and -idle-timeout must be positive")
	}
	return nil
}
