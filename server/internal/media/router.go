package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"hash/maphash"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/udpio"
)

// wireOverhead is added to every packet's size when charging the ingress token bucket
// (IPv4 + UDP headers), so floods of tiny packets are limited too.
const wireOverhead = 28

// Registrar applies media bindings in the control plane (implemented by
// room.Registry). The router calls it only from its control goroutine, never from
// the goroutine that forwards media.
type Registrar interface {
	// Register binds the member owning token to addr. It returns false for unknown
	// tokens.
	Register(token protocol.Token, addr netip.AddrPort) bool
	// Unbind clears the member's binding if it is still addr.
	Unbind(token protocol.Token, addr netip.AddrPort) bool
}

// Config holds the data-plane limits.
type Config struct {
	// IngressBytesPerSec limits what one client may send (media and NACKs, plus 28
	// bytes of IP/UDP header per packet). The budget belongs to the client, not to a
	// membership, so leaving and rejoining does not refill it. 0 disables the limit.
	IngressBytesPerSec int
	IngressBurstBytes  int
	MixerDelayFrames   int // per-sender jitter buffer in front of the mixer
	AudioFECK          int // parity group size for mixed streams; 0 disables FEC
	// RegistersPerSec limits all REGISTER packets with a valid token together, after
	// the per-member and per-source limits; 0 disables this limit.
	RegistersPerSec int
	// RTXBytesPerSec limits the retransmissions one client may request; it applies
	// even when IngressBytesPerSec is 0 and must be positive when NACK is on.
	RTXBytesPerSec int
	RTXBurstBytes  int
	// RebindsPerSec and RebindBurst limit how often one member's binding may be made
	// or moved (after the return-routability check).
	RebindsPerSec float64
	RebindBurst   int
	// BindingTimeout drops a binding that has had no keepalive for this long, so the
	// server stops sending to an address that no longer asks for media. 0 keeps
	// bindings until the member leaves.
	BindingTimeout time.Duration
	// UDPBatch is the most datagrams read per recvmmsg call; the video copies they
	// produce go out with sendmmsg. 1 (or 0) reads and writes one datagram per
	// system call, as the server did before batching.
	UDPBatch int
	// UDPGSO sends each sender's video packets of one read to each receiver as UDP
	// GSO messages (needs UDPBatch > 1 and Linux 4.18 or later).
	UDPGSO bool
	// NACKBufferPackets is how many recent video packets the server keeps per sender
	// to retransmit when a receiver sends a NACK (rounded up to a power of two);
	// 0 turns retransmission off.
	NACKBufferPackets int
	// Logger receives warnings from the data plane (read errors, GSO turned off);
	// nil discards them.
	Logger *slog.Logger
}

// DefaultConfig returns the server defaults (8 Mbit/s per client, 256 KiB burst).
func DefaultConfig() Config {
	return Config{
		IngressBytesPerSec: 8_000_000 / 8,
		IngressBurstBytes:  256 << 10,
		MixerDelayFrames:   2,
		AudioFECK:          2,
		RegistersPerSec:    2000,
		RTXBytesPerSec:     8_000_000 / 8,
		RTXBurstBytes:      256 << 10,
		RebindsPerSec:      1,
		RebindBurst:        4,
		BindingTimeout:     15 * time.Second,
		UDPBatch:           64,
		UDPGSO:             true,
		NACKBufferPackets:  256,
	}
}

// Fixed REGISTER limits. Clients send one REGISTER every 5 s as a keepalive, and one
// every 200 ms while registering.
const (
	memberRegisterRate  = 10 // per member, whatever the source address
	memberRegisterBurst = 20
	sourceRegisterRate  = 20 // per source IP and port, whatever the token
	sourceRegisterBurst = 40
	// sourceBuckets is the size of the per-source table, keyed by IP and port (so
	// each socket of a host has its own budget; the server-wide limit bounds them
	// all). Sources share slots by a keyed hash, so the table is bounded and a
	// collision only makes the limit stricter for the sources involved.
	sourceBuckets = 4096
	// nonceEpoch: a challenge nonce is accepted in the epoch it was issued in and the
	// next one.
	nonceEpoch = 30 * time.Second
	// bindQueue is how many verified binding changes may wait for the control
	// goroutine; more are dropped (the client retries).
	bindQueue = 256
	// minRebuildInterval spaces routing-table rebuilds, so a burst of control-plane
	// changes costs one rebuild.
	minRebuildInterval = 2 * time.Millisecond
	// consentInterval is how often bindings are checked against BindingTimeout (more
	// often for timeouts under 3 s, which tests use).
	consentInterval = time.Second
	// bucketRetention keeps a client's budgets after its last membership ends, so a
	// quick leave and rejoin does not start with full buckets.
	bucketRetention = 10 * time.Second
)

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

// participant is the data-plane state of one room membership. The buckets are used
// only by the Serve goroutine; boundAddr only by the rebuild goroutine.
type participant struct {
	id        uint32 // the client's ID
	audioSSRC uint32
	bucket    *tokenBucket // the client's ingress budget, shared by its memberships
	jb        *jitterBuffer
	down      downstream
	rtx       *rtxRing     // the participant's recent video packets; nil without NACK
	rtxBucket *tokenBucket // the client's retransmission budget, shared likewise
	regBucket tokenBucket  // REGISTERs carrying this member's token
	rebinds   tokenBucket  // verified binding changes
	lastSeen  atomic.Int64 // unix ns of the last keepalive from the bound address
	boundAddr netip.AddrPort
}

type route struct {
	p       *participant
	kind    kind
	addr    netip.AddrPort
	room    *room.RoomView
	targets []udpio.Dest // video: the other registered members
	self    udpio.Dest   // the participant's own address, for retransmissions
}

// binding is a member's entry in the token map: what a REGISTER is checked against.
type binding struct {
	p    *participant
	addr netip.AddrPort // zero while unbound
}

// roomRoutes is the part of a routing table built from one RoomView. A rebuild reuses
// it as long as the view's RoomView pointer is unchanged.
type roomRoutes struct {
	view *room.RoomView
}

// routeTable is immutable once published; the Serve goroutine loads it atomically.
type routeTable struct {
	seq     uint64 // of the view it was built from
	bySSRC  map[uint32]*route
	byToken map[protocol.Token]*binding
	rooms   map[uint32]*roomRoutes
}

// clientBuckets are one client's budgets, kept for bucketRetention after its last
// membership ends.
type clientBuckets struct {
	ingress, rtx *tokenBucket
	users        int
	idleSince    time.Time
}

type bindRequest struct {
	token protocol.Token
	addr  netip.AddrPort
	txID  uint64
}

// Router reads every datagram of the media port and forwards or mixes it according to
// a routing table that is swapped atomically. The Serve goroutine never takes a lock
// that the control plane holds: REGISTERs are checked against the table's token map,
// verified binding changes go to a control goroutine through a bounded queue, and the
// table is rebuilt on a separate goroutine (only the rooms that changed, at most once
// every 2 ms). Video forwarding takes no lock at all; audio takes the sender's jitter
// buffer mutex, which it shares only with its own room's mixer.
type Router struct {
	conn  *net.UDPConn
	io    *udpio.Conn
	batch int // datagrams per read
	nack  int // retransmission buffer per sender, in packets (0 = no NACK)
	reg   Registrar
	cfg   Config
	hooks Hooks
	stats Stats
	table atomic.Pointer[routeTable]

	// Owned by the Serve goroutine: the video packets of the read being handled,
	// grouped by sender, the writer their copies go out through, and the REGISTER
	// limits and nonce MAC.
	runs       []videoRun
	nruns      int
	forwarded  int // packets in runs that have receivers, for the ForwardLatency hook
	out        *udpio.Writer
	lost       []uint16 // scratch for NACK handling
	regBucket  tokenBucket
	srcBuckets []tokenBucket
	srcSeed    maphash.Seed
	mac        hash.Hash
	macIn      [protocol.TokenSize + 16 + 2 + 8]byte
	macOut     [sha256.Size]byte

	binds   chan bindRequest
	pending atomic.Pointer[room.View] // newest view from the registry
	kick    chan struct{}
	done    chan struct{}
	workers sync.WaitGroup

	syncMu   sync.Mutex
	syncCond *sync.Cond
	built    uint64 // seq of the published table
	stopped  bool

	mu      sync.Mutex // guards parts, buckets, rooms and closed
	parts   map[uint32]*participant
	buckets map[uint32]*clientBuckets
	rooms   map[uint32]*mixRoom
	closed  bool
	mixers  sync.WaitGroup
}

// NewRouter creates a router for conn and starts its control and rebuild goroutines.
// Call Serve to start forwarding, connect the registry with
// registry.Observe(router.OnView), and Close to stop everything.
func NewRouter(conn *net.UDPConn, reg Registrar, cfg Config, hooks Hooks) *Router {
	batch := max(1, cfg.UDPBatch)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	opts := udpio.Options{Batching: batch > 1, GSO: cfg.UDPGSO, OnGSOOff: func() {
		if cfg.Logger != nil {
			cfg.Logger.Warn("UDP GSO refused by the kernel; sending one datagram per message from now on")
		}
	}}
	r := &Router{
		conn:       conn,
		io:         udpio.New(conn, opts),
		batch:      batch,
		reg:        reg,
		cfg:        cfg,
		hooks:      hooks,
		regBucket:  makeTokenBucket(float64(cfg.RegistersPerSec), max(1, cfg.RegistersPerSec)),
		srcBuckets: make([]tokenBucket, sourceBuckets),
		srcSeed:    maphash.MakeSeed(),
		mac:        hmac.New(sha256.New, key),
		binds:      make(chan bindRequest, bindQueue),
		kick:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		parts:      make(map[uint32]*participant),
		buckets:    make(map[uint32]*clientBuckets),
		rooms:      make(map[uint32]*mixRoom),
	}
	for i := range r.srcBuckets {
		r.srcBuckets[i] = makeTokenBucket(sourceRegisterRate, sourceRegisterBurst)
	}
	r.syncCond = sync.NewCond(&r.syncMu)
	if cfg.NACKBufferPackets > 0 {
		// A buffer no larger than one read could never answer (see rtxRing.get).
		r.nack = max(cfg.NACKBufferPackets, 2*batch)
	}
	r.out = r.io.NewWriter()
	r.table.Store(&routeTable{
		bySSRC:  map[uint32]*route{},
		byToken: map[protocol.Token]*binding{},
		rooms:   map[uint32]*roomRoutes{},
	})
	r.workers.Add(2)
	go func() {
		defer r.workers.Done()
		r.rebuildLoop()
	}()
	go func() {
		defer r.workers.Done()
		r.controlLoop()
	}()
	return r
}

// Batching reports whether the router reads and writes with recvmmsg and sendmmsg.
func (r *Router) Batching() bool { return r.io.Batching() }

// GSO reports whether the router currently sends video with UDP GSO.
func (r *Router) GSO() bool { return r.io.GSO() }

// videoRun is the video packets one sender had in a read, fragments and parity packets
// apart: a sender's full fragments all have one length and its parity packets another,
// so each list goes to a receiver as one GSO message (or a few).
type videoRun struct {
	rt     *route
	frags  [][]byte
	parity [][]byte
}

// run returns the videoRun of rt in the current read, starting one if needed.
// Packets of one sender usually arrive back to back, so the search starts at the end.
func (r *Router) run(rt *route) *videoRun {
	for i := r.nruns - 1; i >= 0; i-- {
		if r.runs[i].rt == rt {
			return &r.runs[i]
		}
	}
	if r.nruns == len(r.runs) {
		r.runs = append(r.runs, videoRun{})
	}
	vr := &r.runs[r.nruns]
	r.nruns++
	vr.rt = rt
	return vr
}

// Stats returns the router's counters.
func (r *Router) Stats() *Stats { return &r.stats }

// LocalAddr returns the media socket address.
func (r *Router) LocalAddr() netip.AddrPort {
	return r.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Serve reads datagrams until the socket is closed. It handles the datagrams of each
// read in order and sends the video copies they produce together at the end.
func (r *Router) Serve() error {
	ms := make([]udpio.Message, r.batch)
	for i := range ms {
		ms[i].Buf = make([]byte, 2048)
	}
	backoff := time.Duration(0)
	for {
		n, err := r.io.ReadBatch(ms)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A persistent error (for example ENOBUFS or ENOMEM) must not turn into a
			// busy loop: back off from 1 ms up to 1 s while reads keep failing.
			r.stats.ReadErrors.Add(1)
			backoff = min(max(2*backoff, time.Millisecond), time.Second)
			if r.cfg.Logger != nil {
				r.cfg.Logger.Warn("media read failed", "err", err, "retry_in", backoff)
			}
			select {
			case <-r.done:
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		r.stats.ReadCalls.Add(1)
		now := time.Now()
		for i := range n {
			r.handle(ms[i].Buf[:ms[i].N], ms[i].Addr, now)
		}
		r.flush(now)
	}
}

// flush sends the copies of the video packets collected since the last flush:
// for each sender and receiver, the sender's fragments in order, then its parity
// packets. received is when the packets were read.
func (r *Router) flush(received time.Time) {
	for i := range r.nruns {
		vr := &r.runs[i]
		for t := range vr.rt.targets {
			d := &vr.rt.targets[t]
			for _, pkt := range vr.frags {
				r.out.Add(pkt, d, uint8(OutVideo))
			}
			for _, pkt := range vr.parity {
				r.out.Add(pkt, d, uint8(OutVideo))
			}
		}
		clear(vr.frags)
		clear(vr.parity)
		vr.rt, vr.frags, vr.parity = nil, vr.frags[:0], vr.parity[:0]
	}
	r.nruns = 0
	if r.out.Len() == 0 {
		return
	}
	res := r.out.Flush()
	r.stats.sent(&res)
	r.stats.VideoSendCalls.Add(uint64(res.Calls))
	r.stats.VideoSendMessages.Add(uint64(res.Messages))
	if r.hooks.ForwardLatency != nil {
		d := time.Since(received)
		for range r.forwarded {
			r.hooks.ForwardLatency(d)
		}
	}
	r.forwarded = 0
}

// Close stops the mixers and the router's goroutines and closes the socket, which
// ends Serve.
func (r *Router) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	rooms := r.rooms
	r.rooms = map[uint32]*mixRoom{}
	r.mu.Unlock()
	close(r.done)
	r.syncMu.Lock()
	r.stopped = true
	r.syncCond.Broadcast()
	r.syncMu.Unlock()
	for _, mr := range rooms {
		close(mr.stop)
	}
	r.mixers.Wait()
	r.workers.Wait()
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
	case protocol.IsNACK(pkt):
		r.handleNACK(pkt, src, now)
	case protocol.IsRTP(pkt) && len(pkt) >= protocol.RTPHeaderSize:
		r.handleRTP(pkt, src, now)
	default:
		r.stats.drop(DropMalformed)
	}
}

// handleRegister answers a REGISTER without leaving the Serve goroutine. A keepalive
// from the bound address is a lookup in the table's token map and an ACK. A REGISTER
// from any other address gets a challenge carrying a nonce bound to the token, the
// address and the time; only a REGISTER from that address that echoes the nonce
// (return routability, as in STUN/ICE) is queued to the control goroutine, which
// moves the binding and sends the final ACK.
func (r *Router) handleRegister(pkt []byte, src netip.AddrPort, now time.Time) {
	var reg protocol.Register
	if err := protocol.ParseRegister(pkt, &reg); err != nil {
		r.stats.drop(DropMalformed)
		return
	}
	b := r.table.Load().byToken[reg.Token]
	if b == nil {
		r.stats.drop(DropBadToken)
		return
	}
	if !b.p.regBucket.allow(1, now) || !r.sourceBucket(src).allow(1, now) || !r.regBucket.allow(1, now) {
		r.stats.drop(DropRegisterLimited)
		return
	}
	r.stats.in(InRegister, len(pkt))
	if src == b.addr {
		b.p.lastSeen.Store(now.UnixNano())
		r.sendAck(&protocol.RegisterAck{TxID: reg.TxID, Observed: src}, OutRegisterAck)
		return
	}
	nonce := r.nonce(reg.Token, src, now, 0)
	if !reg.HasNonce || (!sameNonce(reg.Nonce, nonce) && !sameNonce(reg.Nonce, r.nonce(reg.Token, src, now, -1))) {
		r.sendAck(&protocol.RegisterAck{TxID: reg.TxID, Observed: src, Challenge: true, Nonce: nonce}, OutRegisterChallenge)
		return
	}
	if !b.p.rebinds.allow(1, now) {
		r.stats.drop(DropRebindLimited)
		return
	}
	select {
	case r.binds <- bindRequest{token: reg.Token, addr: src, txID: reg.TxID}:
	default:
		r.stats.drop(DropBindQueueFull)
	}
}

// sourceBucket returns the REGISTER budget of src's slot.
func (r *Router) sourceBucket(src netip.AddrPort) *tokenBucket {
	var h maphash.Hash
	h.SetSeed(r.srcSeed)
	a := src.Addr().As16()
	_, _ = h.Write(a[:])
	_ = h.WriteByte(byte(src.Port() >> 8))
	_ = h.WriteByte(byte(src.Port()))
	return &r.srcBuckets[h.Sum64()%sourceBuckets]
}

// nonce is the challenge for token at addr in the epoch of now (plus offset epochs):
// the first 8 bytes of HMAC-SHA256 under a key chosen at startup.
func (r *Router) nonce(token protocol.Token, addr netip.AddrPort, now time.Time, offset int64) protocol.Nonce {
	epoch := now.UnixNano()/int64(nonceEpoch) + offset
	in := r.macIn[:]
	copy(in, token[:])
	a := addr.Addr().As16()
	copy(in[protocol.TokenSize:], a[:])
	binary.BigEndian.PutUint16(in[protocol.TokenSize+16:], addr.Port())
	binary.BigEndian.PutUint64(in[protocol.TokenSize+18:], uint64(epoch))
	r.mac.Reset()
	_, _ = r.mac.Write(in)
	sum := r.mac.Sum(r.macOut[:0])
	var n protocol.Nonce
	copy(n[:], sum)
	return n
}

func sameNonce(a, b protocol.Nonce) bool { return subtle.ConstantTimeCompare(a[:], b[:]) == 1 }

// sendAck writes a REGISTER_ACK. The Serve and control goroutines both use it;
// *net.UDPConn is safe for concurrent use.
func (r *Router) sendAck(ack *protocol.RegisterAck, kind OutKind) {
	var out [64]byte
	n, err := ack.MarshalTo(out[:])
	if err != nil {
		return
	}
	if _, err := r.conn.WriteToUDPAddrPort(out[:n], ack.Observed); err != nil {
		r.stats.drop(DropWriteError)
		return
	}
	r.stats.out(kind, n)
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
		r.stats.drop(DropIngressLimited)
		return
	}
	var h protocol.RTPHeader
	payload, err := protocol.ParseRTP(pkt, &h)
	if err != nil {
		r.stats.drop(DropMalformed)
		return
	}
	if rt.kind == kindVideo {
		r.forwardVideo(rt, pkt, payload, pt, h.SequenceNumber)
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

// forwardVideo collects a video fragment or its parity packet for forwarding,
// unchanged, to every other registered member of the sender's room (SFU forwarding,
// no re-encoding) at the next flush. pkt must stay untouched until then.
func (r *Router) forwardVideo(rt *route, pkt, payload []byte, pt uint8, seq uint16) {
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
	if len(rt.targets) == 0 {
		return
	}
	if rt.p.rtx != nil {
		rt.p.rtx.store(pkt, seq)
	}
	vr := r.run(rt)
	if pt == protocol.PTVideo {
		vr.frags = append(vr.frags, pkt)
	} else {
		vr.parity = append(vr.parity, pkt)
	}
	r.forwarded++
}

// handleNACK answers a receiver's generic NACK by sending it the listed packets of
// another member's video stream again, from that sender's retransmission buffer.
// The NACK must come from a registered participant's address, as media does, and
// name a video stream in the same room; the retransmitted bytes count against the
// requester's own retransmission budget, so a receiver cannot make the server send
// more than that.
func (r *Router) handleNACK(pkt []byte, src netip.AddrPort, now time.Time) {
	var n protocol.NACK
	if err := protocol.ParseNACK(pkt, &n); err != nil {
		r.stats.drop(DropMalformed)
		return
	}
	table := r.table.Load()
	req := table.bySSRC[n.SenderSSRC]
	switch {
	case req == nil:
		r.stats.drop(DropUnknownSSRC)
		return
	case req.addr != src:
		r.stats.drop(DropAddrMismatch)
		return
	case !req.room.Relaying(now):
		r.stats.drop(DropP2PMode)
		return
	case !req.p.bucket.allow(len(pkt)+wireOverhead, now):
		r.stats.drop(DropNACKLimited)
		return
	}
	media := table.bySSRC[n.MediaSSRC]
	if media == nil || media.kind != kindVideo || media.room != req.room || media.p == req.p ||
		media.p.rtx == nil || !req.self.AddrPort().IsValid() {
		r.stats.drop(DropBadPayload)
		return
	}
	r.stats.in(InNACK, len(pkt))
	r.lost = n.AppendLost(r.lost[:0])
	for _, seq := range r.lost {
		b := media.p.rtx.get(seq, r.batch)
		if b == nil {
			r.stats.RTXMisses.Add(1)
			continue
		}
		if !req.p.rtxBucket.allow(len(b)+wireOverhead, now) {
			r.stats.drop(DropRTXBudget)
			return
		}
		r.out.Add(b, &req.self, uint8(OutVideoRTX))
	}
}

// OnView receives a new registry view. The registry calls it with its lock held, so
// it only records the view and wakes the rebuild goroutine.
func (r *Router) OnView(v *room.View) {
	r.pending.Store(v)
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Sync waits until the routing table reflects the newest view the registry has
// published, or the router is closed.
func (r *Router) Sync() {
	v := r.pending.Load()
	if v == nil {
		return
	}
	r.syncMu.Lock()
	for r.built < v.Seq && !r.stopped {
		r.syncCond.Wait()
	}
	r.syncMu.Unlock()
}

// rebuildLoop rebuilds the routing table after views arrive, at most once every
// minRebuildInterval; changes that arrive meanwhile are handled by one rebuild.
func (r *Router) rebuildLoop() {
	var last time.Time
	for {
		select {
		case <-r.done:
			return
		case <-r.kick:
		}
		if wait := minRebuildInterval - time.Since(last); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-r.done:
				t.Stop()
				return
			case <-t.C:
			}
		}
		last = time.Now()
		r.rebuild(last)
	}
}

// rebuild builds the routing table for the newest view. Rooms whose RoomView is
// unchanged keep their routes; the others are rebuilt, along with their mixer
// snapshots.
func (r *Router) rebuild(now time.Time) {
	v := r.pending.Load()
	if v == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	prev := r.table.Load()
	if v.Seq > prev.seq {
		next := &routeTable{
			seq:     v.Seq,
			bySSRC:  maps.Clone(prev.bySSRC),
			byToken: maps.Clone(prev.byToken),
			rooms:   make(map[uint32]*roomRoutes, len(v.Rooms)),
		}
		live := make(map[uint32]*room.RoomView, len(v.Rooms))
		for _, rv := range v.Rooms {
			live[rv.ID] = rv
		}
		for id, rr := range prev.rooms {
			if live[id] == nil {
				r.removeRoom(next, rr.view, nil, now)
				if mr := r.rooms[id]; mr != nil {
					delete(r.rooms, id)
					close(mr.stop)
				}
			}
		}
		for _, rv := range v.Rooms {
			if old := prev.rooms[rv.ID]; old != nil && old.view == rv {
				next.rooms[rv.ID] = old
				continue
			}
			next.rooms[rv.ID] = r.buildRoom(next, prev.rooms[rv.ID], rv, now)
			r.stats.RoomRebuilds.Add(1)
		}
		for id, cb := range r.buckets {
			if cb.users == 0 && now.Sub(cb.idleSince) > bucketRetention {
				delete(r.buckets, id)
			}
		}
		r.table.Store(next)
		r.stats.TableRebuilds.Add(1)
	}
	r.syncMu.Lock()
	r.built = max(r.built, v.Seq)
	r.syncCond.Broadcast()
	r.syncMu.Unlock()
}

// removeRoom deletes the routes of rv's members from next and forgets the members
// that are not in keep.
func (r *Router) removeRoom(next *routeTable, rv *room.RoomView, keep map[uint32]bool, now time.Time) {
	for _, mv := range rv.Members {
		delete(next.bySSRC, mv.AudioSSRC)
		delete(next.bySSRC, mv.VideoSSRC)
		delete(next.byToken, mv.Token)
		if keep[mv.AudioSSRC] {
			continue
		}
		if p := r.parts[mv.AudioSSRC]; p != nil {
			delete(r.parts, mv.AudioSSRC)
			if cb := r.buckets[p.id]; cb != nil {
				if cb.users--; cb.users == 0 {
					cb.idleSince = now
				}
			}
		}
	}
}

// buildRoom replaces old's routes in next with routes for rv and updates the room's
// mixer.
func (r *Router) buildRoom(next *routeTable, old *roomRoutes, rv *room.RoomView, now time.Time) *roomRoutes {
	if old != nil {
		keep := make(map[uint32]bool, len(rv.Members))
		for _, mv := range rv.Members {
			keep[mv.AudioSSRC] = true
		}
		r.removeRoom(next, old.view, keep, now)
	}
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
			cb := r.clientBuckets(mv.ID)
			p = &participant{
				id:        mv.ID,
				audioSSRC: mv.AudioSSRC,
				bucket:    cb.ingress,
				jb:        newJitterBuffer(r.cfg.MixerDelayFrames, &r.stats),
				down:      downstream{silent: true},
				rtx:       newRTXRing(r.nack),
				rtxBucket: cb.rtx,
				regBucket: makeTokenBucket(memberRegisterRate, memberRegisterBurst),
				rebinds:   makeTokenBucket(r.cfg.RebindsPerSec, max(1, r.cfg.RebindBurst)),
			}
			r.parts[mv.AudioSSRC] = p
		}
		if p.boundAddr != mv.Addr {
			// A new binding counts as fresh until its first keepalive is due.
			p.boundAddr = mv.Addr
			p.lastSeen.Store(now.UnixNano())
		}
		snap.members[j] = mixMember{p: p, addr: mv.Addr}
		targets := make([]udpio.Dest, 0, len(rv.Members)-1)
		for _, other := range rv.Members {
			if other.ID != mv.ID && other.Addr.IsValid() {
				targets = append(targets, r.io.Dest(other.Addr))
			}
		}
		var self udpio.Dest
		if mv.Addr.IsValid() {
			self = r.io.Dest(mv.Addr)
		}
		next.bySSRC[mv.AudioSSRC] = &route{p: p, kind: kindAudio, addr: mv.Addr, room: rv, self: self}
		next.bySSRC[mv.VideoSSRC] = &route{
			p: p, kind: kindVideo, addr: mv.Addr, room: rv, targets: targets, self: self,
		}
		next.byToken[mv.Token] = &binding{p: p, addr: mv.Addr}
	}
	mr.snap.Store(snap)
	return &roomRoutes{view: rv}
}

// clientBuckets returns client id's budgets, creating them for a new client, and
// counts one more membership using them. r.mu must be held.
func (r *Router) clientBuckets(id uint32) *clientBuckets {
	cb := r.buckets[id]
	if cb == nil {
		cb = &clientBuckets{
			ingress: newTokenBucket(float64(r.cfg.IngressBytesPerSec), r.cfg.IngressBurstBytes),
			rtx:     newTokenBucket(float64(r.cfg.RTXBytesPerSec), r.cfg.RTXBurstBytes),
		}
		r.buckets[id] = cb
	}
	cb.users++
	return cb
}

// controlLoop applies verified binding changes and expires bindings whose
// keepalives stopped. It is the only goroutine of the router that calls the
// registry.
func (r *Router) controlLoop() {
	every := consentInterval
	if t := r.cfg.BindingTimeout; t > 0 {
		every = min(every, max(10*time.Millisecond, t/3))
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return
		case req := <-r.binds:
			r.bind(req)
		case now := <-tick.C:
			r.expire(now)
		}
	}
}

// bind moves the binding in the registry, waits until the routing table has it and
// sends the final REGISTER_ACK.
func (r *Router) bind(req bindRequest) {
	if !r.reg.Register(req.token, req.addr) {
		return // the member left meanwhile
	}
	r.Sync()
	r.stats.Bindings.Add(1)
	r.sendAck(&protocol.RegisterAck{TxID: req.txID, Observed: req.addr}, OutRegisterAck)
}

// expire drops the bindings that have had no keepalive for BindingTimeout.
func (r *Router) expire(now time.Time) {
	if r.cfg.BindingTimeout <= 0 {
		return
	}
	limit := now.Add(-r.cfg.BindingTimeout).UnixNano()
	for tok, b := range r.table.Load().byToken {
		if b.addr.IsValid() && b.p.lastSeen.Load() < limit && r.reg.Unbind(tok, b.addr) {
			r.stats.BindingsExpired.Add(1)
		}
	}
}

func randomUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
