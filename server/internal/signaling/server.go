// Package signaling is the TCP control server. Each connection has one reader
// goroutine, which decodes requests and applies them to the room registry, and one
// writer goroutine, which drains a bounded outbox. The registry queues replies and
// events with a non-blocking send, so a slow client can never stall a room: when its
// outbox is full the connection is closed.
package signaling

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// Config holds per-connection limits.
type Config struct {
	HelloTimeout   time.Duration // time allowed between accept and hello
	IdleTimeout    time.Duration // connection closed after this long without a message
	WriteTimeout   time.Duration // per flush
	OutboxSize     int           // queued frames before a client counts as too slow
	RequestsPerSec float64       // sustained request rate per connection
	RequestBurst   int
	MaxConns       int
}

// DefaultConfig returns the server defaults. Clients ping every 15 s, well inside the
// 60 s idle timeout.
func DefaultConfig() Config {
	return Config{
		HelloTimeout:   10 * time.Second,
		IdleTimeout:    60 * time.Second,
		WriteTimeout:   10 * time.Second,
		OutboxSize:     256,
		RequestsPerSec: 50,
		RequestBurst:   100,
		MaxConns:       1024,
	}
}

// Server accepts control connections.
type Server struct {
	reg *room.Registry
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	ln     net.Listener
	conns  map[*conn]struct{}
	closed bool
	wg     sync.WaitGroup

	active atomic.Int64
	total  atomic.Uint64
	slow   atomic.Uint64
}

// New creates a control server for reg.
func New(reg *room.Registry, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{reg: reg, cfg: cfg, log: log, conns: make(map[*conn]struct{})}
}

// ActiveConns returns the number of open control connections.
func (s *Server) ActiveConns() int64 { return s.active.Load() }

// TotalConns returns the number of connections accepted so far.
func (s *Server) TotalConns() uint64 { return s.total.Load() }

// SlowDisconnects returns how many connections were closed because their outbox
// overflowed.
func (s *Server) SlowDisconnects() uint64 { return s.slow.Load() }

// Serve accepts connections on ln until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		s.accept(nc)
	}
}

func (s *Server) accept(nc net.Conn) {
	s.mu.Lock()
	if s.closed || len(s.conns) >= s.cfg.MaxConns {
		s.mu.Unlock()
		if frame, err := protocol.EncodeFrame(protocol.Error{Type: protocol.TypeError, Code: protocol.CodeLimitReached, Message: "too many connections"}); err == nil {
			_ = nc.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = nc.Write(frame)
		}
		_ = nc.Close()
		return
	}
	c := &conn{
		s:      s,
		nc:     nc,
		out:    make(chan []byte, s.cfg.OutboxSize),
		closed: make(chan struct{}),
		bucket: newRequestBucket(s.cfg.RequestsPerSec, s.cfg.RequestBurst),
	}
	s.conns[c] = struct{}{}
	s.wg.Add(2)
	s.mu.Unlock()
	s.active.Add(1)
	s.total.Add(1)
	s.log.Debug("control connection opened", "remote", nc.RemoteAddr().String())
	go c.writeLoop()
	go c.readLoop()
}

// Shutdown closes the listener, tells every member that its room is closing, flushes
// the outboxes and waits for all connections to finish (or ctx to expire).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.mu.Unlock()
	s.reg.CloseAll()
	s.mu.Lock()
	for c := range s.conns {
		c.close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			_ = c.nc.Close()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *Server) forget(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.active.Add(-1)
}

// conn is one control connection.
type conn struct {
	s         *Server
	nc        net.Conn
	out       chan []byte
	closeOnce sync.Once
	closed    chan struct{}
	client    *room.Client
	bucket    *requestBucket
}

// Send implements room.Notifier. It never blocks: a client whose outbox is full is
// too slow to follow its room and gets disconnected.
func (c *conn) Send(frame []byte) {
	select {
	case <-c.closed:
		return
	default:
	}
	select {
	case c.out <- frame:
	default:
		c.s.slow.Add(1)
		c.s.log.Warn("closing slow control connection", "remote", c.nc.RemoteAddr().String())
		c.close()
	}
}

func (c *conn) close() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *conn) send(msg any) {
	frame, err := protocol.EncodeFrame(msg)
	if err != nil {
		c.s.log.Error("encode control message", "err", err)
		return
	}
	c.Send(frame)
}

func (c *conn) sendError(seq *int64, code, message string) {
	c.send(protocol.Error{Type: protocol.TypeError, Seq: seq, Code: code, Message: message})
}

func (c *conn) readLoop() {
	defer c.s.wg.Done()
	defer func() {
		if c.client != nil {
			c.s.reg.Goodbye(c.client)
		}
		c.close()
	}()
	br := bufio.NewReader(c.nc)
	var buf []byte
	_ = c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.HelloTimeout))
	for {
		body, err := protocol.ReadFrame(br, buf)
		if err != nil {
			if errors.Is(err, protocol.ErrFrameTooLarge) || errors.Is(err, protocol.ErrEmptyFrame) {
				c.sendError(nil, protocol.CodeTooLarge, err.Error())
			}
			return
		}
		buf = body[:0]
		timeout := c.s.cfg.IdleTimeout
		if c.client == nil {
			timeout = c.s.cfg.HelloTimeout
		}
		_ = c.nc.SetReadDeadline(time.Now().Add(timeout))
		req, err := protocol.DecodeRequest(body)
		if err != nil {
			c.sendError(nil, protocol.CodeBadRequest, "malformed message")
			continue
		}
		if !c.bucket.allow(time.Now()) {
			c.sendError(req.Seq, protocol.CodeRateLimited, "too many requests")
			continue
		}
		if !c.dispatch(&req) {
			return
		}
	}
}

// dispatch applies one request. It returns false when the connection must close.
func (c *conn) dispatch(req *protocol.Request) bool {
	reg := c.s.reg
	if req.Type == protocol.TypeHello {
		if c.client != nil {
			c.sendError(req.Seq, protocol.CodeBadRequest, "hello already received")
			return true
		}
		if req.Version != protocol.ControlVersion {
			c.sendError(req.Seq, protocol.CodeUnsupportedVersion, "server speaks protocol version 1")
			return false
		}
		client, err := reg.Hello(req.Name, c)
		if err != nil {
			c.replyErr(req.Seq, err)
			return true
		}
		c.client = client
		c.send(protocol.Welcome{
			Type:         protocol.TypeWelcome,
			Seq:          req.Seq,
			ClientID:     client.ID,
			Version:      protocol.ControlVersion,
			ServerTimeMS: time.Now().UnixMilli(),
			MediaPort:    reg.MediaPort(),
		})
		c.s.log.Info("client connected", "client", client.ID, "name", client.Name, "remote", c.nc.RemoteAddr().String())
		return true
	}
	if c.client == nil {
		c.sendError(req.Seq, protocol.CodeNotHello, "send hello first")
		return true
	}
	var err error
	switch req.Type {
	case protocol.TypeListRooms:
		c.send(protocol.RoomList{Type: protocol.TypeRoomList, Seq: req.Seq, Rooms: reg.ListRooms()})
	case protocol.TypeCreateRoom:
		err = reg.CreateRoom(c.client, req.Title, req.Seq)
	case protocol.TypeJoinRoom:
		err = reg.JoinRoom(c.client, req.RoomID, req.Seq)
	case protocol.TypeLeaveRoom:
		err = reg.LeaveRoom(c.client, req.Seq)
	case protocol.TypeCancelRoom:
		err = reg.CancelRoom(c.client, req.Seq)
	case protocol.TypeChat:
		err = reg.Chat(c.client, req.Text)
	case protocol.TypeMediaState:
		if req.Video == nil || req.Audio == nil {
			c.sendError(req.Seq, protocol.CodeBadRequest, "media_state needs video and audio")
			return true
		}
		err = reg.SetMediaState(c.client, *req.Video, *req.Audio)
	case protocol.TypePing:
		c.send(protocol.Pong{Type: protocol.TypePong, Seq: req.Seq, ServerTimeMS: time.Now().UnixMilli()})
	default:
		c.sendError(req.Seq, protocol.CodeBadRequest, "unknown message type")
	}
	if err != nil {
		c.replyErr(req.Seq, err)
	}
	return true
}

func (c *conn) replyErr(seq *int64, err error) {
	var re *room.Error
	if errors.As(err, &re) {
		c.sendError(seq, re.Code, re.Message)
		return
	}
	c.s.log.Error("request failed", "err", err)
	c.sendError(seq, protocol.CodeInternal, "internal error")
}

func (c *conn) writeLoop() {
	defer c.s.wg.Done()
	defer c.s.forget(c)
	bw := bufio.NewWriter(c.nc)
	write := func(frame []byte) bool {
		_, err := bw.Write(frame)
		return err == nil
	}
	for {
		select {
		case frame := <-c.out:
			_ = c.nc.SetWriteDeadline(time.Now().Add(c.s.cfg.WriteTimeout))
			ok := write(frame)
			for ok && len(c.out) > 0 {
				ok = write(<-c.out)
			}
			if !ok || bw.Flush() != nil {
				c.close()
				_ = c.nc.Close()
				return
			}
		case <-c.closed:
			// Best effort: deliver what is already queued (for example room_closed on
			// shutdown) before closing the socket.
			_ = c.nc.SetWriteDeadline(time.Now().Add(time.Second))
			for len(c.out) > 0 && write(<-c.out) {
			}
			_ = bw.Flush()
			_ = c.nc.Close()
			return
		}
	}
}

// requestBucket limits the request rate of one connection. Only the connection's
// reader goroutine uses it.
type requestBucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newRequestBucket(rate float64, burst int) *requestBucket {
	return &requestBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (b *requestBucket) allow(now time.Time) bool {
	if b.rate <= 0 {
		return true
	}
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
