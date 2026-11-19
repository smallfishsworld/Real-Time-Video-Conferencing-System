package media

import (
	"encoding/binary"
	"math"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
)

// MixInterval is the audio frame duration and therefore the mixer tick.
const MixInterval = 20 * time.Millisecond

// maxMixerLag: when the mixer is this many ticks late it skips ahead instead of
// trying to catch up, so a stall never turns into a burst of stale audio.
const maxMixerLag = 5

const maxMixedPacket = protocol.RTPHeaderSize + 4*protocol.MaxCSRC + protocol.AudioFrameBytes

// saturate clamps a 32-bit sum to the int16 range.
func saturate(v int32) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}

// accumulate adds f into total.
func accumulate(total *[frameSamples]int32, f *audioFrame) {
	for i, v := range f {
		total[i] += int32(v)
	}
}

// mixMinus writes saturate(total - own) into dst, where total is the exact 32-bit
// sum of every active sender and own is the receiver's own frame (nil if the receiver
// is not an active sender). Computing the room total once and subtracting each
// receiver's own contribution makes a tick O(senders + receivers) instead of
// O(senders x receivers).
func mixMinus(dst *audioFrame, total *[frameSamples]int32, own *audioFrame) {
	if own == nil {
		for i, v := range total {
			dst[i] = saturate(v)
		}
		return
	}
	for i, v := range total {
		dst[i] = saturate(v - int32(own[i]))
	}
}

// packetWriter is the subset of *net.UDPConn the media plane writes with.
type packetWriter interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

// downstream is the state of one receiver's mixed stream. Only the mixer goroutine
// of the receiver's room touches it.
type downstream struct {
	seq    uint16
	silent bool
	fec    protocol.FECEncoder
}

type mixMember struct {
	p    *participant
	addr netip.AddrPort
}

type mixSnapshot struct {
	room    *room.RoomView
	members []mixMember
}

// mixRoom runs the 20 ms audio mixer of one room in its own goroutine.
type mixRoom struct {
	id    uint32
	ssrc  uint32
	snap  atomic.Pointer[mixSnapshot]
	stop  chan struct{}
	conn  packetWriter
	stats *Stats
	hooks *Hooks
	fecK  int

	// Scratch state owned by the mixer goroutine.
	ts     uint32
	frames []audioFrame
	active []bool
	total  [frameSamples]int32
	out    audioFrame
	pkt    [maxMixedPacket]byte
	parity [protocol.MaxSendDatagram]byte
}

func newMixRoom(id, ssrc uint32, conn packetWriter, stats *Stats, hooks *Hooks, fecK int, ts0 uint32) *mixRoom {
	return &mixRoom{
		id:    id,
		ssrc:  ssrc,
		stop:  make(chan struct{}),
		conn:  conn,
		stats: stats,
		hooks: hooks,
		fecK:  fecK,
		ts:    ts0,
	}
}

func (m *mixRoom) run() {
	next := time.Now().Add(MixInterval)
	timer := time.NewTimer(MixInterval)
	defer timer.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-timer.C:
		}
		now := time.Now()
		m.tick(now)
		next = next.Add(MixInterval)
		if lag := now.Sub(next); lag > maxMixerLag*MixInterval {
			skipped := int(lag / MixInterval)
			m.ts += uint32(skipped * frameSamples)
			next = next.Add(time.Duration(skipped) * MixInterval)
			m.stats.MixerOverruns.Add(uint64(skipped))
		}
		timer.Reset(time.Until(next))
	}
}

// tick produces one 20 ms frame for every receiver of the room and advances the
// room's RTP clock.
func (m *mixRoom) tick(now time.Time) {
	m.mix(now)
	m.ts += frameSamples
}

func (m *mixRoom) mix(now time.Time) {
	snap := m.snap.Load()
	if snap == nil || !snap.room.Relaying(now) {
		return
	}
	start := time.Now()
	n := len(snap.members)
	if cap(m.frames) < n {
		m.frames = make([]audioFrame, n)
		m.active = make([]bool, n)
	}
	m.frames, m.active = m.frames[:n], m.active[:n]
	m.total = [frameSamples]int32{}
	nActive := 0
	for i, mm := range snap.members {
		ok, _ := mm.p.jb.pop(&m.frames[i])
		m.active[i] = ok
		if ok {
			nActive++
			accumulate(&m.total, &m.frames[i])
		}
	}
	for i, mm := range snap.members {
		if !mm.addr.IsValid() {
			continue
		}
		d := &mm.p.down
		others := nActive
		var own *audioFrame
		if m.active[i] {
			others--
			own = &m.frames[i]
		}
		if others == 0 {
			d.silent = true
			continue
		}
		mixMinus(&m.out, &m.total, own)
		m.send(snap, i, mm, d)
	}
	m.stats.MixerTicks.Add(1)
	if m.hooks.MixTick != nil {
		m.hooks.MixTick(time.Since(start))
	}
}

// send encodes m.out for receiver i and writes it, followed by a parity packet when
// the receiver's FEC group is complete.
func (m *mixRoom) send(snap *mixSnapshot, i int, mm mixMember, d *downstream) {
	h := protocol.RTPHeader{
		Marker:         d.silent,
		PayloadType:    protocol.PTAudio,
		SequenceNumber: d.seq,
		Timestamp:      m.ts,
		SSRC:           m.ssrc,
	}
	for j, other := range snap.members {
		if j != i && m.active[j] && h.CSRCCount < protocol.MaxCSRC {
			h.CSRC[h.CSRCCount] = other.p.audioSSRC
			h.CSRCCount++
		}
	}
	hn, err := h.MarshalTo(m.pkt[:])
	if err != nil {
		return
	}
	for j, v := range m.out {
		binary.BigEndian.PutUint16(m.pkt[hn+2*j:], uint16(v))
	}
	n := hn + protocol.AudioFrameBytes
	d.seq++
	d.silent = false
	m.write(m.pkt[:n], mm.addr, OutAudio)
	if m.fecK <= 0 {
		return
	}
	if d.fec.Add(m.pkt[:n]) != nil {
		return
	}
	if d.fec.Len() >= m.fecK {
		pn, err := d.fec.Flush(m.parity[:], m.ssrc, d.seq)
		if err != nil {
			return
		}
		d.seq++
		m.write(m.parity[:pn], mm.addr, OutAudioFEC)
	}
}

func (m *mixRoom) write(b []byte, addr netip.AddrPort, kind OutKind) {
	if _, err := m.conn.WriteToUDPAddrPort(b, addr); err != nil {
		m.stats.drop(DropWriteError)
		return
	}
	m.stats.out(kind, len(b))
}
