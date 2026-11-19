// Package app wires the registry, the media router, the control server and the
// metrics endpoint into one server that can run in-process (tests) or from
// cmd/confserver.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/media"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/metrics"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/signaling"
)

// Config is the complete server configuration.
type Config struct {
	ControlAddr  string // TCP control listener
	MediaAddr    string // UDP media socket
	MetricsAddr  string // HTTP /metrics (and pprof); empty disables it
	EnablePprof  bool
	SocketBuffer int // SO_RCVBUF/SO_SNDBUF request for the media socket, bytes

	Room      room.Config
	Media     media.Config
	Signaling signaling.Config
	Logger    *slog.Logger
}

// DefaultConfig listens on loopback only; pass explicit addresses to serve a LAN.
// Signaling.MaxConns is 0, which Start turns into Room.MaxClients plus
// ConnHeadroom.
func DefaultConfig() Config {
	sig := signaling.DefaultConfig()
	sig.MaxConns = 0
	return Config{
		ControlAddr:  "127.0.0.1:6100",
		MediaAddr:    "127.0.0.1:6100",
		MetricsAddr:  "127.0.0.1:6101",
		SocketBuffer: 4 << 20,
		Room:         room.DefaultConfig(),
		Media:        media.DefaultConfig(),
		Signaling:    sig,
	}
}

// ConnHeadroom is how many control connections beyond Room.MaxClients the server
// accepts by default: connections that have not sent hello yet, or are about to be
// refused with limit_reached.
const ConnHeadroom = 64

// effective fills in the derived settings.
func (cfg Config) effective() Config {
	if cfg.Signaling.MaxConns <= 0 {
		cfg.Signaling.MaxConns = cfg.Room.MaxClients + ConnHeadroom
	}
	if cfg.Media.Logger == nil {
		cfg.Media.Logger = cfg.Logger
	}
	return cfg
}

// Server is a running conference server.
type Server struct {
	Registry *room.Registry
	Router   *media.Router
	Control  *signaling.Server
	Metrics  *metrics.Metrics

	log       *slog.Logger
	controlLn net.Listener
	metricsLn net.Listener
	httpSrv   *http.Server
	wg        sync.WaitGroup
	errMu     sync.Mutex
	err       error
	done      chan struct{} // closed when a serving goroutine fails
	doneOnce  sync.Once
	cfg       Config
}

// Start binds all sockets and starts serving.
func Start(cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
		cfg.Logger = log
	}
	cfg = cfg.effective()
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.MediaAddr)
	if err != nil {
		return nil, fmt.Errorf("media address: %w", err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen media: %w", err)
	}
	if cfg.SocketBuffer > 0 {
		_ = udp.SetReadBuffer(cfg.SocketBuffer)
		_ = udp.SetWriteBuffer(cfg.SocketBuffer)
	}
	ctl, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("listen control: %w", err)
	}

	s := &Server{log: log, controlLn: ctl, Metrics: metrics.New(), done: make(chan struct{}), cfg: cfg}
	s.Registry = room.NewRegistry(cfg.Room)
	s.Router = media.NewRouter(udp, s.Registry, cfg.Media, s.Metrics.Hooks())
	s.Registry.SetMediaPort(int(s.Router.LocalAddr().Port()))
	s.Registry.Observe(s.Router.OnView)
	s.Control = signaling.New(s.Registry, cfg.Signaling, log)
	s.Metrics.Attach(s.Router, s.Registry, s.Control)

	if cfg.MetricsAddr != "" {
		s.metricsLn, err = net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			_ = ctl.Close()
			_ = s.Router.Close()
			return nil, fmt.Errorf("listen metrics: %w", err)
		}
		s.httpSrv = &http.Server{Handler: s.Metrics.Handler(cfg.EnablePprof), ReadHeaderTimeout: 5 * time.Second}
		s.goServe("metrics", func() error {
			if err := s.httpSrv.Serve(s.metricsLn); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	s.goServe("media", s.Router.Serve)
	s.goServe("control", func() error { return s.Control.Serve(ctl) })
	log.Info("server started",
		"control", ctl.Addr().String(),
		"media", s.Router.LocalAddr().String(),
		"metrics", s.MetricsAddr(),
		"pprof", cfg.EnablePprof,
		"p2p", cfg.Room.P2PEnabled,
		"udp_batching", s.Router.Batching(),
		"udp_gso", s.Router.GSO(),
		"max_clients", cfg.Room.MaxClients,
		"max_conns", cfg.Signaling.MaxConns)
	return s, nil
}

// Config returns the configuration the server runs with, derived settings filled in.
func (s *Server) Config() Config { return s.cfg }

// Done is closed when a serving goroutine (media, control or metrics) fails; Err then
// returns its error. The process should shut down rather than keep running without it.
func (s *Server) Done() <-chan struct{} { return s.done }

// Err returns the first serving error, or nil.
func (s *Server) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Server) goServe(name string, fn func() error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := fn(); err != nil {
			s.log.Error("serve failed", "component", name, "err", err)
			s.errMu.Lock()
			if s.err == nil {
				s.err = fmt.Errorf("%s: %w", name, err)
			}
			s.errMu.Unlock()
			s.doneOnce.Do(func() { close(s.done) })
		}
	}()
}

// ControlAddr returns the bound control address.
func (s *Server) ControlAddr() string { return s.controlLn.Addr().String() }

// MediaAddr returns the bound media address.
func (s *Server) MediaAddr() string { return s.Router.LocalAddr().String() }

// MetricsAddr returns the bound metrics address, or "" when disabled.
func (s *Server) MetricsAddr() string {
	if s.metricsLn == nil {
		return ""
	}
	return s.metricsLn.Addr().String()
}

// Shutdown closes every room (members get room_closed), stops the media plane and
// the metrics server, and waits for the serving goroutines.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.Control.Shutdown(ctx)
	if cerr := s.Router.Close(); err == nil {
		err = cerr
	}
	if s.httpSrv != nil {
		if herr := s.httpSrv.Shutdown(ctx); err == nil {
			err = herr
		}
	}
	s.wg.Wait()
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if err == nil {
		err = s.err
	}
	return err
}
