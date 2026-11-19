// Package room holds the control-plane state: connected clients, rooms, membership,
// the host's rights and the per-room media mode. Every mutation happens under one
// mutex (control traffic is low-rate) and, after each one, the registry publishes an
// immutable View that the media plane reads without locks.
package room

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Limits on user-supplied strings.
const (
	MaxNameLen  = 32
	MaxTitleLen = 64
	MaxChatLen  = 2000
)

// Notifier delivers encoded control frames to one client connection. Send is called
// with the registry lock held, so it must never block.
type Notifier interface {
	Send(frame []byte)
}

// Config holds the registry limits and policies.
type Config struct {
	MaxRoomSize int           // members per room
	MaxRooms    int           // rooms on the server
	MaxClients  int           // connected clients on the server
	P2PEnabled  bool          // allow the two-member P2P mode
	RelayGrace  time.Duration // how long the server keeps relaying after relay -> p2p
	MediaPort   int           // UDP port advertised to clients
	Now         func() time.Time
}

// DefaultConfig returns the server defaults.
func DefaultConfig() Config {
	return Config{
		MaxRoomSize: 16,
		MaxRooms:    256,
		MaxClients:  1024,
		P2PEnabled:  true,
		RelayGrace:  time.Second,
		Now:         time.Now,
	}
}

// Error is a request failure with a protocol error code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Client is one connected control session.
type Client struct {
	ID   uint32
	Name string

	notifier Notifier
	member   *member
}

// InRoom reports the room the client is in, or 0.
func (c *Client) InRoom() uint32 {
	if c.member == nil {
		return 0
	}
	return c.member.room.id
}

type member struct {
	client    *Client
	room      *Room
	audioSSRC uint32
	videoSSRC uint32
	token     protocol.Token
	addr      netip.AddrPort
	video     bool
	audio     bool
}

// Room is one meeting.
type Room struct {
	id         uint32
	title      string
	hostID     uint32
	hostName   string
	created    time.Time
	mixerSSRC  uint32
	members    []*member
	mode       Mode
	modeKey    string
	relayUntil time.Time
}

// Registry is the control-plane state machine. Its methods are safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	cfg       Config
	clients   map[uint32]*Client
	rooms     map[uint32]*Room
	tokens    map[protocol.Token]*member
	ssrcs     map[uint32]struct{}
	lastID    uint32
	lastRoom  uint32
	observers []func(*View)
	view      *View
}

// NewRegistry creates an empty registry.
func NewRegistry(cfg Config) *Registry {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Registry{
		cfg:     cfg,
		clients: make(map[uint32]*Client),
		rooms:   make(map[uint32]*Room),
		tokens:  make(map[protocol.Token]*member),
		ssrcs:   make(map[uint32]struct{}),
	}
	r.view = r.buildView()
	return r
}

// Observe registers fn to receive every new View. fn runs with the registry lock held
// and must not call back into the registry. It is called once immediately.
func (r *Registry) Observe(fn func(*View)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observers = append(r.observers, fn)
	fn(r.view)
}

// View returns the current immutable view.
func (r *Registry) View() *View {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.view
}

// SetMediaPort sets the UDP port advertised in `joined` and `welcome`.
func (r *Registry) SetMediaPort(port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.MediaPort = port
}

// MediaPort returns the advertised UDP port.
func (r *Registry) MediaPort() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.MediaPort
}

// Hello registers a new client after a valid hello message.
func (r *Registry) Hello(name string, n Notifier) (*Client, error) {
	name, err := cleanText(name, 1, MaxNameLen, false)
	if err != nil {
		return nil, errorf(protocol.CodeBadRequest, "name: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.clients) >= r.cfg.MaxClients {
		return nil, errorf(protocol.CodeLimitReached, "server has %d clients", len(r.clients))
	}
	r.lastID++
	c := &Client{ID: r.lastID, Name: name, notifier: n}
	r.clients[c.ID] = c
	return c, nil
}

// Goodbye removes a disconnected client, leaving its room if it was in one.
func (r *Registry) Goodbye(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c.ID]; !ok {
		return
	}
	if c.member != nil {
		r.removeMember(c.member)
		r.publish()
	}
	delete(r.clients, c.ID)
}

// ListRooms returns the open rooms ordered by id.
func (r *Registry) ListRooms() []protocol.RoomInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.RoomInfo, 0, len(r.rooms))
	for _, rm := range r.rooms {
		out = append(out, protocol.RoomInfo{
			RoomID:          rm.id,
			Title:           rm.title,
			HostName:        rm.hostName,
			Participants:    len(rm.members),
			MaxParticipants: r.cfg.MaxRoomSize,
			Mode:            rm.mode.String(),
			CreatedMS:       rm.created.UnixMilli(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })
	return out
}

// CreateRoom creates a room with c as host and joins c to it. The `joined` reply
// (carrying seq) is queued to c before any event about the room.
func (r *Registry) CreateRoom(c *Client, title string, seq *int64) error {
	title, err := cleanText(title, 0, MaxTitleLen, false)
	if err != nil {
		return errorf(protocol.CodeBadRequest, "title: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member != nil {
		return errorf(protocol.CodeAlreadyInRoom, "already in room %d", c.member.room.id)
	}
	if len(r.rooms) >= r.cfg.MaxRooms {
		return errorf(protocol.CodeLimitReached, "server has %d rooms", len(r.rooms))
	}
	r.lastRoom++
	rm := &Room{
		id:        r.lastRoom,
		title:     title,
		hostID:    c.ID,
		hostName:  c.Name,
		created:   r.cfg.Now(),
		mixerSSRC: r.newSSRC(),
		mode:      ModeRelay,
	}
	if rm.title == "" {
		rm.title = fmt.Sprintf("%s's room", c.Name)
	}
	r.rooms[rm.id] = rm
	r.addMember(c, rm, seq)
	return nil
}

// JoinRoom adds c to an existing room.
func (r *Registry) JoinRoom(c *Client, roomID uint32, seq *int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member != nil {
		return errorf(protocol.CodeAlreadyInRoom, "already in room %d", c.member.room.id)
	}
	rm, ok := r.rooms[roomID]
	if !ok {
		return errorf(protocol.CodeRoomNotFound, "no room %d", roomID)
	}
	if len(rm.members) >= r.cfg.MaxRoomSize {
		return errorf(protocol.CodeRoomFull, "room %d is full (%d members)", roomID, len(rm.members))
	}
	r.addMember(c, rm, seq)
	return nil
}

// LeaveRoom removes c from its room. The room closes when its last member leaves.
func (r *Registry) LeaveRoom(c *Client, seq *int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member == nil {
		return errorf(protocol.CodeNotInRoom, "not in a room")
	}
	roomID := c.member.room.id
	r.removeMember(c.member)
	r.send(c, protocol.Left{Type: protocol.TypeLeft, Seq: seq, RoomID: roomID})
	r.publish()
	return nil
}

// CancelRoom closes c's room for everyone. Only the host may cancel.
func (r *Registry) CancelRoom(c *Client, seq *int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member == nil {
		return errorf(protocol.CodeNotInRoom, "not in a room")
	}
	rm := c.member.room
	if rm.hostID != c.ID {
		return errorf(protocol.CodeNotHost, "only the host can cancel room %d", rm.id)
	}
	r.closeRoom(rm, protocol.ReasonCancelled, c, seq)
	r.publish()
	return nil
}

// CloseAll closes every room, used on server shutdown.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range r.rooms {
		r.closeRoom(rm, protocol.ReasonServerShutdown, nil, nil)
	}
	r.publish()
}

// Chat broadcasts a chat message from c to every member of its room, stamped with the
// sender's name and the server clock.
func (r *Registry) Chat(c *Client, text string) error {
	text, err := cleanText(text, 1, MaxChatLen, true)
	if err != nil {
		return errorf(protocol.CodeBadRequest, "text: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member == nil {
		return errorf(protocol.CodeNotInRoom, "not in a room")
	}
	rm := c.member.room
	r.broadcast(rm, protocol.Chat{
		Type:         protocol.TypeChat,
		RoomID:       rm.id,
		FromID:       c.ID,
		FromName:     c.Name,
		Text:         text,
		ServerTimeMS: r.cfg.Now().UnixMilli(),
	})
	return nil
}

// SetMediaState records whether c's camera and microphone are on.
func (r *Registry) SetMediaState(c *Client, video, audio bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.member == nil {
		return errorf(protocol.CodeNotInRoom, "not in a room")
	}
	c.member.video, c.member.audio = video, audio
	r.broadcastRoster(c.member.room)
	r.publish()
	return nil
}

// Register binds the UDP source address of a REGISTER packet to the member owning
// token. It returns false for unknown tokens.
func (r *Registry) Register(token protocol.Token, addr netip.AddrPort) bool {
	if !addr.IsValid() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.tokens[token]
	if !ok {
		return false
	}
	if m.addr == addr {
		return true
	}
	first := !m.addr.IsValid()
	m.addr = addr
	if first {
		r.broadcastRoster(m.room)
	}
	r.refreshMode(m.room, nil)
	r.publish()
	return true
}

func (r *Registry) addMember(c *Client, rm *Room, seq *int64) {
	m := &member{
		client:    c,
		room:      rm,
		audioSSRC: r.newSSRC(),
		videoSSRC: r.newSSRC(),
		token:     r.newToken(),
		video:     true,
		audio:     true,
	}
	rm.members = append(rm.members, m)
	r.tokens[m.token] = m
	c.member = m
	r.send(c, protocol.Joined{
		Type:            protocol.TypeJoined,
		Seq:             seq,
		RoomID:          rm.id,
		Title:           rm.title,
		Host:            rm.hostID == c.ID,
		ParticipantID:   c.ID,
		AudioSSRC:       m.audioSSRC,
		VideoSSRC:       m.videoSSRC,
		MixerSSRC:       rm.mixerSSRC,
		Token:           m.token.String(),
		MediaPort:       r.cfg.MediaPort,
		MaxParticipants: r.cfg.MaxRoomSize,
	})
	r.broadcastRoster(rm)
	r.refreshMode(rm, m)
	r.publish()
}

// removeMember takes m out of its room and closes the room if it became empty.
func (r *Registry) removeMember(m *member) {
	rm := m.room
	for i, x := range rm.members {
		if x == m {
			rm.members = append(rm.members[:i], rm.members[i+1:]...)
			break
		}
	}
	r.forget(m)
	if len(rm.members) == 0 {
		delete(r.rooms, rm.id)
		delete(r.ssrcs, rm.mixerSSRC)
		return
	}
	if rm.hostID == m.client.ID {
		rm.hostID = 0
	}
	r.broadcastRoster(rm)
	r.refreshMode(rm, nil)
}

func (r *Registry) closeRoom(rm *Room, reason string, requester *Client, seq *int64) {
	for _, m := range rm.members {
		msg := protocol.RoomClosed{Type: protocol.TypeRoomClosed, RoomID: rm.id, Reason: reason}
		if m.client == requester {
			msg.Seq = seq
		}
		r.send(m.client, msg)
		r.forget(m)
	}
	rm.members = nil
	delete(r.rooms, rm.id)
	delete(r.ssrcs, rm.mixerSSRC)
}

func (r *Registry) forget(m *member) {
	delete(r.tokens, m.token)
	delete(r.ssrcs, m.audioSSRC)
	delete(r.ssrcs, m.videoSSRC)
	m.client.member = nil
}

// refreshMode recomputes the room's mode and announces it when it changed. A newly
// joined member always receives the current mode.
func (r *Registry) refreshMode(rm *Room, joined *member) {
	addrs := make([]netip.AddrPort, len(rm.members))
	for i, m := range rm.members {
		addrs[i] = m.addr
	}
	mode := DecideMode(len(rm.members), addrs, r.cfg.P2PEnabled)
	key := mode.String()
	if mode == ModeP2P {
		key += "|" + rm.members[0].addr.String() + "|" + rm.members[1].addr.String()
	}
	changed := key != rm.modeKey
	if changed {
		if rm.mode == ModeRelay && mode == ModeP2P {
			rm.relayUntil = r.cfg.Now().Add(r.cfg.RelayGrace)
		}
		rm.mode = mode
		rm.modeKey = key
	}
	for i, m := range rm.members {
		if !changed && m != joined {
			continue
		}
		msg := protocol.Mode{Type: protocol.TypeMode, RoomID: rm.id, Mode: mode.String()}
		if mode == ModeP2P {
			other := rm.members[1-i]
			msg.Peer = &protocol.Peer{
				ParticipantID: other.client.ID,
				AudioSSRC:     other.audioSSRC,
				VideoSSRC:     other.videoSSRC,
				Addr:          other.addr.String(),
			}
		}
		r.send(m.client, msg)
	}
}

func (r *Registry) broadcastRoster(rm *Room) {
	roster := protocol.Roster{
		Type:         protocol.TypeRoster,
		RoomID:       rm.id,
		HostID:       rm.hostID,
		Participants: make([]protocol.RosterEntry, len(rm.members)),
	}
	for i, m := range rm.members {
		roster.Participants[i] = protocol.RosterEntry{
			ParticipantID: m.client.ID,
			Name:          m.client.Name,
			AudioSSRC:     m.audioSSRC,
			VideoSSRC:     m.videoSSRC,
			Video:         m.video,
			Audio:         m.audio,
			Registered:    m.addr.IsValid(),
		}
	}
	r.broadcast(rm, roster)
}

// broadcast encodes msg once and queues it to every member of rm.
func (r *Registry) broadcast(rm *Room, msg any) {
	frame, err := protocol.EncodeFrame(msg)
	if err != nil {
		return
	}
	for _, m := range rm.members {
		m.client.notifier.Send(frame)
	}
}

func (r *Registry) send(c *Client, msg any) {
	frame, err := protocol.EncodeFrame(msg)
	if err != nil {
		return
	}
	c.notifier.Send(frame)
}

func (r *Registry) newSSRC() uint32 {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		s := binary.BigEndian.Uint32(b[:])
		if _, used := r.ssrcs[s]; s != 0 && !used {
			r.ssrcs[s] = struct{}{}
			return s
		}
	}
}

func (r *Registry) newToken() protocol.Token {
	var t protocol.Token
	if _, err := rand.Read(t[:]); err != nil {
		panic(err)
	}
	return t
}

// cleanText trims s and checks its length in runes. Control characters are rejected,
// except newlines and tabs when multiline is set.
func cleanText(s string, minLen, maxLen int, multiline bool) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", errors.New("invalid UTF-8")
	}
	n := utf8.RuneCountInString(s)
	if n < minLen || n > maxLen {
		return "", fmt.Errorf("length %d outside %d..%d", n, minLen, maxLen)
	}
	for _, ch := range s {
		if unicode.IsControl(ch) && !(multiline && (ch == '\n' || ch == '\t')) {
			return "", errors.New("control characters are not allowed")
		}
	}
	return s, nil
}
