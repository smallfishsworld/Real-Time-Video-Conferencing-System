package room

import (
	"net/netip"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Mode is a room's media mode.
type Mode uint8

// Media modes.
const (
	ModeRelay Mode = iota
	ModeP2P
)

func (m Mode) String() string {
	if m == ModeP2P {
		return protocol.ModeP2P
	}
	return protocol.ModeRelay
}

// DecideMode returns the media mode for a room with members registered at addrs
// (a zero AddrPort means not registered yet). Two registered members talk P2P when
// P2P is enabled; every other case uses the relay.
func DecideMode(members int, addrs []netip.AddrPort, p2pEnabled bool) Mode {
	if !p2pEnabled || members != 2 || len(addrs) != 2 {
		return ModeRelay
	}
	if !addrs[0].IsValid() || !addrs[1].IsValid() {
		return ModeRelay
	}
	return ModeP2P
}

// View is an immutable snapshot of the rooms, published after every change that the
// media plane needs (membership, bindings and modes; not media_state). A room that did
// not change since the previous view keeps the same *RoomView, so the media plane only
// rebuilds the rooms whose pointer changed.
type View struct {
	Seq   uint64      // increases with every published view
	Rooms []*RoomView // ordered by ID
}

// RoomView is the media-relevant state of one room.
type RoomView struct {
	ID         uint32
	Mode       Mode
	RelayUntil time.Time // relay continues until this time after a switch to P2P
	MixerSSRC  uint32
	Members    []MemberView // in join order
}

// Relaying reports whether the server should forward and mix media for the room at now.
func (rv *RoomView) Relaying(now time.Time) bool {
	return rv.Mode == ModeRelay || now.Before(rv.RelayUntil)
}

// MemberView is the media-relevant state of one member.
type MemberView struct {
	ID        uint32 // the client's ID, which outlives the membership
	AudioSSRC uint32
	VideoSSRC uint32
	Token     protocol.Token
	Addr      netip.AddrPort // zero until the member registers
}

// roomView returns rm's view, building it if rm changed since the last one. Lock must
// be held.
func (rm *Room) roomView() *RoomView {
	if rm.view != nil {
		return rm.view
	}
	rv := &RoomView{
		ID:         rm.id,
		Mode:       rm.mode,
		RelayUntil: rm.relayUntil,
		MixerSSRC:  rm.mixerSSRC,
		Members:    make([]MemberView, len(rm.members)),
	}
	for i, m := range rm.members {
		rv.Members[i] = MemberView{
			ID:        m.client.ID,
			AudioSSRC: m.audioSSRC,
			VideoSSRC: m.videoSSRC,
			Token:     m.token,
			Addr:      m.addr,
		}
	}
	rm.view = rv
	return rv
}

// buildView must be called with the lock held. Rooms are kept in ID order.
func (r *Registry) buildView() *View {
	v := &View{Seq: r.seq, Rooms: make([]*RoomView, len(r.order))}
	for i, rm := range r.order {
		v.Rooms[i] = rm.roomView()
	}
	return v
}

// publish builds a new view and hands it to the observers. Only the rooms changed
// since the last view are rebuilt. Lock must be held.
func (r *Registry) publish() {
	r.seq++
	r.view = r.buildView()
	for _, fn := range r.observers {
		fn(r.view)
	}
}

// Stats returns the number of rooms and of members in rooms.
func (v *View) Stats() (rooms, members int) {
	for i := range v.Rooms {
		members += len(v.Rooms[i].Members)
	}
	return len(v.Rooms), members
}
