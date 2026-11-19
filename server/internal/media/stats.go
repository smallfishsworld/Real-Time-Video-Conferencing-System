// Package media is the UDP data plane: address registration, SFU forwarding of video,
// per-room audio mixing, per-sender rate limits and traffic statistics.
package media

import (
	"sync/atomic"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/udpio"
)

// InKind classifies received datagrams.
type InKind int

// Received datagram kinds.
const (
	InRegister InKind = iota
	InVideo
	InVideoFEC
	InAudio
	InAudioFEC
	InNACK // retransmission requests (RTCP generic NACK)
	inKindCount
)

var inKindNames = [inKindCount]string{"register", "video", "video_fec", "audio", "audio_fec", "nack"}

func (k InKind) String() string { return inKindNames[k] }

// OutKind classifies sent datagrams.
type OutKind int

// Sent datagram kinds.
const (
	OutRegisterAck       OutKind = iota // REGISTER_ACKs for a binding (made, moved or kept)
	OutVideo                            // forwarded video fragments and their parity packets
	OutAudio                            // mixed audio frames
	OutAudioFEC                         // parity packets of mixed audio streams
	OutVideoRTX                         // video packets sent again in answer to a NACK
	OutRegisterChallenge                // REGISTER_ACK challenges to a new source address
	outKindCount
)

var outKindNames = [outKindCount]string{
	"register_ack", "video", "audio", "audio_fec", "video_rtx", "register_challenge",
}

func (k OutKind) String() string { return outKindNames[k] }

// DropReason says why a datagram was discarded.
type DropReason int

// Drop reasons, in the order the router checks them.
const (
	DropMalformed       DropReason = iota
	DropOversize                   // larger than protocol.MaxDatagram
	DropBadToken                   // REGISTER with a token no member holds
	DropRegisterLimited            // REGISTER over the member, source or server budget
	DropRebindLimited              // verified binding change over the member's budget
	DropBindQueueFull              // verified binding change while the control queue was full
	DropUnknownSSRC
	DropAddrMismatch // media or NACK not from the SSRC owner's bound address
	DropP2PMode
	DropIngressLimited // media over the sender's ingress budget
	DropNACKLimited    // NACK over the requester's ingress budget
	DropRTXBudget      // retransmission over the requester's retransmission budget
	DropBadPayload
	DropLate
	DropWriteError
	dropReasonCount
)

var dropNames = [dropReasonCount]string{
	"malformed", "oversize", "bad_token", "register_limited", "rebind_limited",
	"bind_queue_full", "unknown_ssrc", "addr_mismatch", "p2p_mode", "ingress_limited",
	"nack_limited", "rtx_budget", "bad_payload", "late", "write_error",
}

func (r DropReason) String() string { return dropNames[r] }

// Stats holds the data-plane counters. All fields are updated atomically, so the
// struct can be read while the router runs.
type Stats struct {
	PacketsIn  [inKindCount]atomic.Uint64
	BytesIn    [inKindCount]atomic.Uint64
	PacketsOut [outKindCount]atomic.Uint64
	BytesOut   [outKindCount]atomic.Uint64
	Drops      [dropReasonCount]atomic.Uint64

	// System calls on the media socket: reads that returned datagrams (recvmmsg
	// returns many per call), sends of forwarded video, and the messages those sends
	// carried (with UDP GSO, one message holds several datagrams).
	ReadCalls         atomic.Uint64
	ReadErrors        atomic.Uint64 // failed reads (other than the socket closing)
	VideoSendCalls    atomic.Uint64
	VideoSendMessages atomic.Uint64

	RTXMisses atomic.Uint64 // NACKed packets no longer (or never) in the sender's buffer

	// Control-plane work done for the media plane, off the Serve goroutine.
	Bindings        atomic.Uint64 // verified bindings applied (first or moved)
	BindingsExpired atomic.Uint64 // bindings dropped after BindingTimeout without keepalives
	TableRebuilds   atomic.Uint64 // routing tables built
	RoomRebuilds    atomic.Uint64 // rooms whose routes were rebuilt (unchanged rooms are reused)

	MixerTicks     atomic.Uint64
	FECRecovered   atomic.Uint64 // upstream audio packets rebuilt from parity
	MixerOverruns  atomic.Uint64 // ticks skipped because the mixer fell behind
	AudioConcealed atomic.Uint64 // sender frames missing when the mixer needed them

	// Jitter-buffer corrections (jitter.go): frames dropped because a sender's buffer
	// stayed above its target, concealed frames inserted because it stayed below, and
	// re-anchors after a run of late packets.
	AudioDrained    atomic.Uint64
	AudioStretched  atomic.Uint64
	AudioReanchored atomic.Uint64
}

func (s *Stats) in(k InKind, n int) {
	s.PacketsIn[k].Add(1)
	s.BytesIn[k].Add(uint64(n))
}

func (s *Stats) out(k OutKind, n int) {
	s.PacketsOut[k].Add(1)
	s.BytesOut[k].Add(uint64(n))
}

func (s *Stats) drop(r DropReason) { s.Drops[r].Add(1) }

// The media plane tags the datagrams it queues on a udpio.Writer with their OutKind.
var _ [udpio.MaxTags - int(outKindCount)]struct{}

// sent counts the datagrams of one Writer flush.
func (s *Stats) sent(res *udpio.Result) {
	for k := range outKindCount {
		if res.Packets[k] > 0 {
			s.PacketsOut[k].Add(uint64(res.Packets[k]))
			s.BytesOut[k].Add(uint64(res.Bytes[k]))
		}
	}
	if res.Failed > 0 {
		s.Drops[DropWriteError].Add(uint64(res.Failed))
	}
}

// Snapshot is a plain copy of Stats, keyed by the label names used in metrics.
type Snapshot struct {
	PacketsIn    map[string]uint64
	BytesIn      map[string]uint64
	PacketsOut   map[string]uint64
	BytesOut     map[string]uint64
	Drops        map[string]uint64
	MixerTicks   uint64
	FECRecovered uint64
}

// Snapshot copies the current counter values.
func (s *Stats) Snapshot() Snapshot {
	snap := Snapshot{
		PacketsIn:  map[string]uint64{},
		BytesIn:    map[string]uint64{},
		PacketsOut: map[string]uint64{},
		BytesOut:   map[string]uint64{},
		Drops:      map[string]uint64{},
	}
	for k := InKind(0); k < inKindCount; k++ {
		snap.PacketsIn[k.String()] = s.PacketsIn[k].Load()
		snap.BytesIn[k.String()] = s.BytesIn[k].Load()
	}
	for k := OutKind(0); k < outKindCount; k++ {
		snap.PacketsOut[k.String()] = s.PacketsOut[k].Load()
		snap.BytesOut[k.String()] = s.BytesOut[k].Load()
	}
	for r := DropReason(0); r < dropReasonCount; r++ {
		snap.Drops[r.String()] = s.Drops[r].Load()
	}
	snap.MixerTicks = s.MixerTicks.Load()
	snap.FECRecovered = s.FECRecovered.Load()
	return snap
}

// InKinds, OutKinds and DropReasons list every label value, for metric exporters.
func InKinds() []InKind {
	out := make([]InKind, inKindCount)
	for i := range out {
		out[i] = InKind(i)
	}
	return out
}

// OutKinds lists every OutKind.
func OutKinds() []OutKind {
	out := make([]OutKind, outKindCount)
	for i := range out {
		out[i] = OutKind(i)
	}
	return out
}

// DropReasons lists every DropReason.
func DropReasons() []DropReason {
	out := make([]DropReason, dropReasonCount)
	for i := range out {
		out[i] = DropReason(i)
	}
	return out
}
