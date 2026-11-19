// Package client is a minimal Go implementation of the client side of the protocol.
// The integration tests and the load generator use it to drive synthetic participants.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Message is the union of all server-to-client message fields.
type Message struct {
	Type    string `json:"type"`
	Seq     *int64 `json:"seq,omitempty"`
	Code    string `json:"code,omitempty"`
	ErrText string `json:"message,omitempty"`

	ClientID     uint32 `json:"client_id,omitempty"`
	Version      int    `json:"version,omitempty"`
	ServerTimeMS int64  `json:"server_time_ms,omitempty"`
	MediaPort    int    `json:"media_port,omitempty"`

	Rooms []protocol.RoomInfo `json:"rooms,omitempty"`

	RoomID          uint32 `json:"room_id,omitempty"`
	Title           string `json:"title,omitempty"`
	Host            bool   `json:"host,omitempty"`
	ParticipantID   uint32 `json:"participant_id,omitempty"`
	AudioSSRC       uint32 `json:"audio_ssrc,omitempty"`
	VideoSSRC       uint32 `json:"video_ssrc,omitempty"`
	MixerSSRC       uint32 `json:"mixer_ssrc,omitempty"`
	Token           string `json:"token,omitempty"`
	MaxParticipants int    `json:"max_participants,omitempty"`

	HostID       uint32                 `json:"host_id,omitempty"`
	Participants []protocol.RosterEntry `json:"participants,omitempty"`

	Mode string         `json:"mode,omitempty"`
	Peer *protocol.Peer `json:"peer,omitempty"`

	FromID   uint32 `json:"from_id,omitempty"`
	FromName string `json:"from_name,omitempty"`
	Text     string `json:"text,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ServerError is an `error` reply.
type ServerError struct {
	Code    string
	Message string
}

func (e *ServerError) Error() string { return fmt.Sprintf("server error %s: %s", e.Code, e.Message) }

// ErrClosed is returned once the control connection has closed.
var ErrClosed = errors.New("client: control connection closed")

func isEvent(t string) bool {
	switch t {
	case protocol.TypeRoster, protocol.TypeMode, protocol.TypeChat, protocol.TypeRoomClosed:
		return true
	}
	return false
}

// Control is a control-plane connection.
type Control struct {
	conn    net.Conn
	wmu     sync.Mutex
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan Message
	events  chan Message
	done    chan struct{}
	err     error
	Welcome Message
}

// Dial connects, says hello and waits for welcome.
func Dial(ctx context.Context, addr, name string) (*Control, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Control{
		conn:    conn,
		pending: make(map[int64]chan Message),
		events:  make(chan Message, 1024),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	w, err := c.Request(ctx, map[string]any{"type": protocol.TypeHello, "name": name, "version": protocol.ControlVersion})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.Welcome = w
	return c, nil
}

// Events returns server-initiated messages (roster, mode, chat, room_closed). The
// channel is buffered; events are dropped if the caller never reads them.
func (c *Control) Events() <-chan Message { return c.events }

// Done is closed when the connection ends.
func (c *Control) Done() <-chan struct{} { return c.done }

// Close closes the connection.
func (c *Control) Close() error { return c.conn.Close() }

// Send writes a message without waiting for a reply.
func (c *Control) Send(msg map[string]any) error {
	frame, err := protocol.EncodeFrame(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.conn.Write(frame)
	return err
}

// Request sends msg with a fresh seq and waits for the reply carrying it.
func (c *Control) Request(ctx context.Context, msg map[string]any) (Message, error) {
	seq := c.seq.Add(1)
	ch := make(chan Message, 1)
	c.mu.Lock()
	c.pending[seq] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}()
	out := make(map[string]any, len(msg)+1)
	for k, v := range msg {
		out[k] = v
	}
	out["seq"] = seq
	if err := c.Send(out); err != nil {
		return Message{}, err
	}
	select {
	case m := <-ch:
		if m.Type == protocol.TypeError {
			return m, &ServerError{Code: m.Code, Message: m.ErrText}
		}
		return m, nil
	case <-c.done:
		return Message{}, ErrClosed
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// WaitEvent returns the next event of type typ that satisfies match (nil matches any),
// discarding other events.
func (c *Control) WaitEvent(ctx context.Context, typ string, match func(Message) bool) (Message, error) {
	for {
		select {
		case m := <-c.events:
			if m.Type == typ && (match == nil || match(m)) {
				return m, nil
			}
		case <-c.done:
			return Message{}, ErrClosed
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
}

func (c *Control) readLoop() {
	defer close(c.done)
	br := bufio.NewReader(c.conn)
	var buf []byte
	for {
		body, err := protocol.ReadFrame(br, buf)
		if err != nil {
			c.err = err
			return
		}
		var m Message
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}
		if m.Seq != nil {
			c.mu.Lock()
			ch := c.pending[*m.Seq]
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
		if isEvent(m.Type) {
			select {
			case c.events <- m:
			default:
			}
		}
	}
}
