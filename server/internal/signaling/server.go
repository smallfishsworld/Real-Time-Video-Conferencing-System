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
	"syscall"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// Config holds per-connection limits.
type Config struct {
	HelloTimeout   time.Duration // time allowed between accept and a successful hello
	IdleTimeout    time.Duration // connection closed after this long without a message
	WriteTimeout   time.Duration // per flush
	OutboxSize     int           // queued frames before a client counts as too slow
	RequestsPerSec float64       // sustained rate of frames per connection, valid or not
	RequestBurst   int
	MaxConns       int
}

// maxMalformed is how many malformed frames in a row close a connection.
const maxMalformed = 3

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

	// mediaGate, when set, is called by a connection's writer before it writes the
	// `joined` reply: it waits until the media plane routes the member's token.
	mediaGate func(protocol.Token)
}

// SetMediaGate makes every `joined` reply wait for wait(token) before it is written,
// so that the client can register its media address at once. Call it before Serve.
func (s *Server) SetMediaGate(wait func(protocol.Token)) { s.mediaGate = wait }

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
	var backoff time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if !temporaryAcceptError(err) {
				return err
			}
			// Out of file descriptors or memory: wait and retry, as net/http does,
			// instead of stopping the control plane for good.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed; retrying", "err", err, "retry_in", backoff)
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		s.accept(nc)
	}
}

// temporaryAcceptError reports whether an Accept error may go away by itself.
func temporaryAcceptError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
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

	disconnectReason string      // set by the reader goroutine before it returns
	slowClosed       atomic.Bool // closed because its outbox overflowed

	// hold is the token of a `joined` reply that is about to be queued; the writer
	// waits until the media plane routes it before writing anything further.
	hold atomic.Pointer[protocol.Token]
}

// HoldUntilRoutable implements room.MediaGate.
func (c *conn) HoldUntilRoutable(token protocol.Token) {
	if c.s.mediaGate != nil {
		c.hold.Store(&token)
	}
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
		c.slowClosed.Store(true)
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

// reply sends msg, the reply to a request with seq; if it cannot be encoded the
// requester gets an error with that seq instead of no answer.
func (c *conn) reply(seq *int64, msg any) {
	frame, err := protocol.EncodeFrame(msg)
	if err != nil {
		c.s.log.Error("encode control reply", "err", err)
		c.sendError(seq, protocol.CodeInternal, "reply could not be encoded")
		return
	}
	c.Send(frame)
}

// roomList builds the room_list reply, cut to fit one control message.
func (c *conn) roomList(seq *int64) protocol.RoomList {
	rooms := c.s.reg.ListRooms()
	list := protocol.RoomList{Type: protocol.TypeRoomList, Seq: seq, Rooms: rooms}
	if _, err := protocol.EncodeFrame(list); err == nil {
		return list
	}
	// Find the longest prefix that fits.
	lo, hi := 0, len(rooms)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		list.Rooms = rooms[:mid]
		list.Truncated, list.Total = true, len(rooms)
		if _, err := protocol.EncodeFrame(list); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	list.Rooms, list.Truncated, list.Total = rooms[:lo], true, len(rooms)
	return list
}

func (c *conn) sendError(seq *int64, code, message string) {
	c.send(protocol.Error{Type: protocol.TypeError, Seq: seq, Code: code, Message: message})
}

func (c *conn) readLoop() {
	defer c.s.wg.Done()
	defer func() {
		if c.slowClosed.Load() {
			c.disconnectReason = "outbox overflow (slow client)"
		}
		if c.client != nil {
			c.s.log.Info("client disconnected", "client", c.client.ID, "reason", c.disconnectReason)
			c.s.reg.Goodbye(c.client)
		} else {
			c.s.log.Debug("control connection closed before hello", "remote", c.nc.RemoteAddr().String(), "reason", c.disconnectReason)
		}
		c.close()
	}()
	br := bufio.NewReader(c.nc)
	var buf []byte
	// One absolute deadline for hello: frames before it do not extend it.
	helloDeadline := time.Now().Add(c.s.cfg.HelloTimeout)
	_ = c.nc.SetReadDeadline(helloDeadline)
	malformed := 0
	for {
		body, err := protocol.ReadFrame(br, buf)
		if err != nil {
			if errors.Is(err, protocol.ErrFrameTooLarge) || errors.Is(err, protocol.ErrEmptyFrame) {
				c.sendError(nil, protocol.CodeTooLarge, err.Error())
			}
			c.disconnectReason = readErrorReason(err)
			return
		}
		buf = body[:0]
		if c.client != nil {
			_ = c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.IdleTimeout))
		}
		// Every frame costs budget before it is decoded, so malformed frames are not
		// free either.
		if !c.bucket.allow(time.Now()) {
			c.sendError(nil, protocol.CodeRateLimited, "too many requests")
			continue
		}
		req, err := protocol.DecodeRequest(body)
		if err != nil {
			c.sendError(nil, protocol.CodeBadRequest, "malformed message")
			if malformed++; malformed >= maxMalformed {
				c.disconnectReason = "malformed messages"
				return
			}
			continue
		}
		malformed = 0
		if !c.dispatch(&req) {
			return
		}
		if c.client != nil {
			_ = c.nc.SetReadDeadline(time.Now().Add(c.s.cfg.IdleTimeout))
		}
	}
}

func readErrorReason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF):
		return "closed by client"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, protocol.ErrFrameTooLarge), errors.Is(err, protocol.ErrEmptyFrame):
		return "bad frame"
	default:
		return "read error"
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
			c.disconnectReason = "unsupported version"
			return false
		}
		client, err := reg.Hello(req.Name, c)
		if err != nil {
			c.replyErr(req.Seq, err)
			var re *room.Error
			if errors.As(err, &re) && re.Code == protocol.CodeLimitReached {
				c.disconnectReason = "server full"
				return false // the server is full: do not keep the connection open
			}
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
		c.reply(req.Seq, c.roomList(req.Seq))
	case protocol.TypeCreateRoom:
		err = reg.CreateRoom(c.client, req.Title, req.Seq)
	case protocol.TypeJoinRoom:
		err = reg.JoinRoom(c.client, req.RoomID, req.Seq)
	case protocol.TypeLeaveRoom:
		err = reg.LeaveRoom(c.client, req.Seq)
	case protocol.TypeCancelRoom:
		err = reg.CancelRoom(c.client, req.Seq)
	case protocol.TypeChat:
		err = reg.Chat(c.client, req.Text, req.Seq)
	case protocol.TypeMediaState:
		if req.Video == nil || req.Audio == nil {
			c.sendError(req.Seq, protocol.CodeBadRequest, "media_state needs video and audio")
			return true
		}
		err = reg.SetMediaState(c.client, *req.Video, *req.Audio, req.Seq)
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
		if tok := c.hold.Swap(nil); tok != nil {
			// The frame is (or follows) a `joined` reply: write it only once the
			// client's first REGISTER would be answered.
			c.s.mediaGate(*tok)
		}
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
