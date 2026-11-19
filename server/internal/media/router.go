package media

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// wireOverhead is added to every packet's size when charging the ingress token bucket
// (IPv4 + UDP headers), so floods of tiny packets are limited too.
const wireOverhead = 28

// Registrar binds registered UDP addresses to participants (implemented by
// room.Registry).
type Registrar interface {
	Register(token protocol.Token, addr netip.AddrPort) bool
}

// Config holds the data-plane limits.
type Config struct {
	IngressBytesPerSec int // per participant; 0 disables the limit
	IngressBurstBytes  int
	MixerDelayFrames   int // per-sender jitter buffer in front of the mixer
	AudioFECK          int // parity group size for mixed streams; 0 disables FEC
	RegistersPerSec    int // all REGISTER packets together; 0 disables the limit
}

// DefaultConfig returns the server defaults (8 Mbit/s per sender, 256 KiB burst).
func DefaultConfig() Config {
	return Config{
		IngressBytesPerSec: 8_000_000 / 8,
		IngressBurstBytes:  256 << 10,
		MixerDelayFrames:   2,
		AudioFECK:          2,
		RegistersPerSec:    2000,
	}
}

// Hooks receive latency observations for metrics. Nil hooks are skipped.
type Hooks struct {
	ForwardLatency func(time.Duration) // receive to last forwarded copy of a video packet
	MixTick        func(time.Duration) // duration of one mixer tick
}

type kind uint8

const (
	kindAudio kind = iota
	kindVideo
)

// participant is the data-plane state of one room membership.
type participant struct {
	id        uint32
	audioSSRC uint32
	bucket    *tokenBucket
	jb        *jitterBuffer
	down      downstream
}

type route struct {
	p       *participant
	kind    kind
	addr    netip.AddrPort
	room    *room.RoomView
	targets []netip.AddrPort // video: the other registered members
}

type routeTable struct {
	bySSRC map[uint32]*route
}

// Router reads every datagram of the media port and forwards or mixes it according to
// a routing table that is rebuilt on each control-plane change and swapped atomically,
// so the per-packet path takes no locks except the sender's own rate limiter.
type Router struct {
	conn  *net.UDPConn
	reg   Registrar
	cfg   Config
	hooks Hooks
	stats Stats
	table atomic.Pointer[routeTable]

	regBucket *tokenBucket

	mu     sync.Mutex // guards parts, rooms and closed
	parts  map[uint32]*participant
	rooms  map[uint32]*mixRoom
	closed bool
	mixers sync.WaitGroup
}

// NewRouter creates a router for conn. Call Serve to start it and connect the
// registry with registry.Observe(router.OnView).
func NewRouter(conn *net.UDPConn, reg Registrar, cfg Config, hooks Hooks) *Router {
	r := &Router{
		conn:      conn,
		reg:       reg,
		cfg:       cfg,
		hooks:     hooks,
		regBucket: newTokenBucket(float64(cfg.RegistersPerSec), max(1, cfg.RegistersPerSec)),
		parts:     make(map[uint32]*participant),
		rooms:     make(map[uint32]*mixRoom),
	}
	r.table.Store(&routeTable{bySSRC: map[uint32]*route{}})
	return r
}

// Stats returns the router's counters.
func (r *Router) Stats() *Stats { return &r.stats }

// LocalAddr returns the media socket address.
func (r *Router) LocalAddr() netip.AddrPort {
	return r.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Serve reads datagrams until the socket is closed.
func (r *Router) Serve() error {
	buf := make([]byte, 2048)
	for {
		n, src, err := r.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		r.handle(buf[:n], src, time.Now())
	}
}

// Close stops the mixers and closes the socket, which ends Serve.
func (r *Router) Close() error {
	r.mu.Lock()
	r.closed = true
	rooms := r.rooms
	r.rooms = map[uint32]*mixRoom{}
	r.mu.Unlock()
	for _, mr := range rooms {
		close(mr.stop)
	}
	r.mixers.Wait()
	return r.conn.Close()
}

func (r *Router) handle(pkt []byte, src netip.AddrPort, now time.Time) {
	switch {
	case len(pkt) > protocol.MaxDatagram:
		r.stats.drop(DropOversize)
	case len(pkt) == 0:
		r.stats.drop(DropMalformed)
	case pkt[0] == protocol.TypeRegister:
		r.handleRegister(pkt, src, now)
	case protocol.IsRTP(pkt) && len(pkt) >= protocol.RTPHeaderSize:
		r.handleRTP(pkt, src, now)
	default:
		r.stats.drop(DropMalformed)
	}
}

func (r *Router) handleRegister(pkt []byte, src netip.AddrPort, now time.Time) {
	var reg protocol.Register
	if err := protocol.ParseRegister(pkt, &reg); err != nil {
		r.stats.drop(DropMalformed)
		return
	}
	if !r.regBucket.allow(1, now) {
		r.stats.drop(DropRateLimited)
		return
	}
	r.stats.in(InRegister, len(pkt))
	if !r.reg.Register(reg.Token, src) {
		r.stats.drop(DropBadToken)
		return
	}
	ack := protocol.RegisterAck{TxID: reg.TxID, Observed: src}
	var out [32]byte
	n, err := ack.MarshalTo(out[:])
	if err != nil {
		return
	}
	if _, err := r.conn.WriteToUDPAddrPort(out[:n], src); err != nil {
		r.stats.drop(DropWriteError)
		return
	}
	r.stats.out(OutRegisterAck, n)
}

func (r *Router) handleRTP(pkt []byte, src netip.AddrPort, now time.Time) {
	ssrc, pt := protocol.PeekSSRC(pkt)
	rt := r.table.Load().bySSRC[ssrc]
	switch {
	case rt == nil:
		r.stats.drop(DropUnknownSSRC)
		return
	case rt.addr != src:
		r.stats.drop(DropAddrMismatch)
		return
	case !rt.room.Relaying(now):
		r.stats.drop(DropP2PMode)
		return
	case !rt.p.bucket.allow(len(pkt)+wireOverhead, now):
		r.stats.drop(DropRateLimited)
		return
	}
	var h protocol.RTPHeader
	payload, err := protocol.ParseRTP(pkt, &h)
	if err != nil {
		r.stats.drop(DropMalformed)
		return
	}
	if rt.kind == kindVideo {
		r.forwardVideo(rt, pkt, payload, pt, now)
		return
	}
	switch pt {
	case protocol.PTAudio:
		switch rt.p.jb.push(pkt, &h, payload) {
		case pushAccepted:
			r.stats.in(InAudio, len(pkt))
		case pushLate:
			r.stats.in(InAudio, len(pkt))
			r.stats.drop(DropLate)
		default:
			r.stats.drop(DropBadPayload)
		}
	case protocol.PTFEC:
		if !rt.p.jb.pushParity(pkt) {
			r.stats.drop(DropBadPayload)
			return
		}
		r.stats.in(InAudioFEC, len(pkt))
	default:
		r.stats.drop(DropBadPayload)
	}
}

// forwardVideo sends a video fragment or its parity packet unchanged to every other
// registered member of the sender's room (SFU forwarding, no re-encoding).
func (r *Router) forwardVideo(rt *route, pkt, payload []byte, pt uint8, now time.Time) {
	switch pt {
	case protocol.PTVideo:
		var v protocol.VideoHeader
		if _, err := protocol.ParseVideo(payload, &v); err != nil {
			r.stats.drop(DropBadPayload)
			return
		}
		r.stats.in(InVideo, len(pkt))
	case protocol.PTFEC:
		var f protocol.FECHeader
		if _, err := protocol.ParseFEC(payload, &f); err != nil {
			r.stats.drop(DropBadPayload)
			return
		}
		r.stats.in(InVideoFEC, len(pkt))
	default:
		r.stats.drop(DropBadPayload)
		return
	}
	for _, dst := range rt.targets {
		if _, err := r.conn.WriteToUDPAddrPort(pkt, dst); err != nil {
			r.stats.drop(DropWriteError)
			continue
		}
		r.stats.out(OutVideo, len(pkt))
	}
	if r.hooks.ForwardLatency != nil && len(rt.targets) > 0 {
		r.hooks.ForwardLatency(time.Since(now))
	}
}

// OnView rebuilds the routing table and the mixers from a registry view. The registry
// calls it with its lock held, so it must not call back into the registry.
func (r *Router) OnView(v *room.View) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	table := &routeTable{bySSRC: make(map[uint32]*route)}
	liveParts := make(map[uint32]bool)
	liveRooms := make(map[uint32]bool)
	for i := range v.Rooms {
		rv := &v.Rooms[i]
		liveRooms[rv.ID] = true
		mr := r.rooms[rv.ID]
		if mr == nil {
			mr = newMixRoom(rv.ID, rv.MixerSSRC, r.conn, &r.stats, &r.hooks, r.cfg.AudioFECK, randomUint32())
			r.rooms[rv.ID] = mr
			r.mixers.Add(1)
			go func() {
				defer r.mixers.Done()
				mr.run()
			}()
		}
		snap := &mixSnapshot{room: rv, members: make([]mixMember, len(rv.Members))}
		for j, mv := range rv.Members {
			p := r.parts[mv.AudioSSRC]
			if p == nil {
				p = &participant{
					id:        mv.ID,
					audioSSRC: mv.AudioSSRC,
					bucket:    newTokenBucket(float64(r.cfg.IngressBytesPerSec), r.cfg.IngressBurstBytes),
					jb:        newJitterBuffer(r.cfg.MixerDelayFrames, &r.stats),
					down:      downstream{silent: true},
				}
				r.parts[mv.AudioSSRC] = p
			}
			liveParts[mv.AudioSSRC] = true
			snap.members[j] = mixMember{p: p, addr: mv.Addr}
			targets := make([]netip.AddrPort, 0, len(rv.Members)-1)
			for _, other := range rv.Members {
				if other.ID != mv.ID && other.Addr.IsValid() {
					targets = append(targets, other.Addr)
				}
			}
			table.bySSRC[mv.AudioSSRC] = &route{p: p, kind: kindAudio, addr: mv.Addr, room: rv}
			table.bySSRC[mv.VideoSSRC] = &route{p: p, kind: kindVideo, addr: mv.Addr, room: rv, targets: targets}
		}
		mr.snap.Store(snap)
	}
	for ssrc := range r.parts {
		if !liveParts[ssrc] {
			delete(r.parts, ssrc)
		}
	}
	for id, mr := range r.rooms {
		if !liveRooms[id] {
			delete(r.rooms, id)
			close(mr.stop)
		}
	}
	r.table.Store(table)
}

func randomUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
