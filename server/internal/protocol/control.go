package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// MaxControlMessage is the largest JSON body accepted on the control connection.
const MaxControlMessage = 64 << 10

// ControlVersion is the protocol version exchanged in hello/welcome.
const ControlVersion = 1

// Control message types (docs/PROTOCOL.md section 1).
const (
	TypeHello      = "hello"
	TypeListRooms  = "list_rooms"
	TypeCreateRoom = "create_room"
	TypeJoinRoom   = "join_room"
	TypeLeaveRoom  = "leave_room"
	TypeCancelRoom = "cancel_room"
	TypeChat       = "chat"
	TypeMediaState = "media_state"
	TypePing       = "ping"

	TypeWelcome    = "welcome"
	TypeRoomList   = "room_list"
	TypeJoined     = "joined"
	TypeLeft       = "left"
	TypeRoster     = "roster"
	TypeMode       = "mode"
	TypeRoomClosed = "room_closed"
	TypePong       = "pong"
	TypeError      = "error"
)

// Media modes announced in `mode` messages and room listings.
const (
	ModeP2P   = "p2p"
	ModeRelay = "relay"
)

// Room closing reasons.
const (
	ReasonCancelled      = "cancelled"
	ReasonServerShutdown = "server_shutdown"
)

// Error codes carried by `error` messages.
const (
	CodeBadRequest         = "bad_request"
	CodeNotHello           = "not_hello"
	CodeUnsupportedVersion = "unsupported_version"
	CodeAlreadyInRoom      = "already_in_room"
	CodeNotInRoom          = "not_in_room"
	CodeRoomNotFound       = "room_not_found"
	CodeRoomFull           = "room_full"
	CodeRoomClosed         = "room_closed"
	CodeNotHost            = "not_host"
	CodeTooLarge           = "too_large"
	CodeRateLimited        = "rate_limited"
	CodeLimitReached       = "limit_reached"
	CodeInternal           = "internal"
)

// Errors returned by the frame reader.
var (
	ErrFrameTooLarge = errors.New("protocol: control message too large")
	ErrEmptyFrame    = errors.New("protocol: empty control message")
	ErrNoType        = errors.New("protocol: control message without type")
)

// ReadFrame reads one length-prefixed control message from r. It reuses buf when it is
// large enough. The returned slice is valid until the next call with the same buf.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if n > MaxControlMessage {
		return nil, ErrFrameTooLarge
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// EncodeFrame marshals msg to JSON and prefixes it with its length.
func EncodeFrame(msg any) ([]byte, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxControlMessage {
		return nil, ErrFrameTooLarge
	}
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	return out, nil
}

// Request is the union of all client-to-server message fields.
type Request struct {
	Type    string `json:"type"`
	Seq     *int64 `json:"seq,omitempty"`
	Name    string `json:"name,omitempty"`
	Version int    `json:"version,omitempty"`
	Title   string `json:"title,omitempty"`
	RoomID  uint32 `json:"room_id,omitempty"`
	Text    string `json:"text,omitempty"`
	Video   *bool  `json:"video,omitempty"`
	Audio   *bool  `json:"audio,omitempty"`
}

// DecodeRequest parses one control message body.
func DecodeRequest(body []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, err
	}
	if req.Type == "" {
		return Request{}, ErrNoType
	}
	return req, nil
}

// Welcome answers hello.
type Welcome struct {
	Type         string `json:"type"`
	Seq          *int64 `json:"seq,omitempty"`
	ClientID     uint32 `json:"client_id"`
	Version      int    `json:"version"`
	ServerTimeMS int64  `json:"server_time_ms"`
	MediaPort    int    `json:"media_port"`
}

// RoomInfo is one entry of a room listing.
type RoomInfo struct {
	RoomID          uint32 `json:"room_id"`
	Title           string `json:"title"`
	HostName        string `json:"host_name"`
	Participants    int    `json:"participants"`
	MaxParticipants int    `json:"max_participants"`
	Mode            string `json:"mode"`
	CreatedMS       int64  `json:"created_ms"`
}

// RoomList answers list_rooms.
type RoomList struct {
	Type  string     `json:"type"`
	Seq   *int64     `json:"seq,omitempty"`
	Rooms []RoomInfo `json:"rooms"`
}

// Joined answers create_room and join_room.
type Joined struct {
	Type            string `json:"type"`
	Seq             *int64 `json:"seq,omitempty"`
	RoomID          uint32 `json:"room_id"`
	Title           string `json:"title"`
	Host            bool   `json:"host"`
	ParticipantID   uint32 `json:"participant_id"`
	AudioSSRC       uint32 `json:"audio_ssrc"`
	VideoSSRC       uint32 `json:"video_ssrc"`
	MixerSSRC       uint32 `json:"mixer_ssrc"`
	Token           string `json:"token"`
	MediaPort       int    `json:"media_port"`
	MaxParticipants int    `json:"max_participants"`
}

// Left answers leave_room.
type Left struct {
	Type   string `json:"type"`
	Seq    *int64 `json:"seq,omitempty"`
	RoomID uint32 `json:"room_id"`
}

// RosterEntry describes one room member.
type RosterEntry struct {
	ParticipantID uint32 `json:"participant_id"`
	Name          string `json:"name"`
	AudioSSRC     uint32 `json:"audio_ssrc"`
	VideoSSRC     uint32 `json:"video_ssrc"`
	Video         bool   `json:"video"`
	Audio         bool   `json:"audio"`
	Registered    bool   `json:"registered"`
}

// Roster is sent to every member whenever membership or media state changes.
type Roster struct {
	Type         string        `json:"type"`
	Seq          *int64        `json:"seq,omitempty"` // only on the requester's copy after media_state
	RoomID       uint32        `json:"room_id"`
	HostID       uint32        `json:"host_id"`
	Participants []RosterEntry `json:"participants"`
}

// Peer is the other member of a P2P room.
type Peer struct {
	ParticipantID uint32 `json:"participant_id"`
	AudioSSRC     uint32 `json:"audio_ssrc"`
	VideoSSRC     uint32 `json:"video_ssrc"`
	Addr          string `json:"addr"`
}

// Mode announces the media mode of a room.
type Mode struct {
	Type   string `json:"type"`
	RoomID uint32 `json:"room_id"`
	Mode   string `json:"mode"`
	Peer   *Peer  `json:"peer,omitempty"`
}

// Chat is a chat message stamped by the server.
type Chat struct {
	Type         string `json:"type"`
	Seq          *int64 `json:"seq,omitempty"` // only on the sender's own copy
	RoomID       uint32 `json:"room_id"`
	FromID       uint32 `json:"from_id"`
	FromName     string `json:"from_name"`
	Text         string `json:"text"`
	ServerTimeMS int64  `json:"server_time_ms"`
}

// RoomClosed tells members that their room no longer exists.
type RoomClosed struct {
	Type   string `json:"type"`
	Seq    *int64 `json:"seq,omitempty"`
	RoomID uint32 `json:"room_id"`
	Reason string `json:"reason"`
}

// Pong answers ping.
type Pong struct {
	Type         string `json:"type"`
	Seq          *int64 `json:"seq,omitempty"`
	ServerTimeMS int64  `json:"server_time_ms"`
}

// Error reports a failed request.
type Error struct {
	Type    string `json:"type"`
	Seq     *int64 `json:"seq,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
