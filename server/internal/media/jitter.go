package media

import (
	"encoding/binary"
	"sync"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

const (
	frameSamples = protocol.AudioSamplesPerFrame
	// jbSlots bounds the per-sender buffer to 16 frames (320 ms).
	jbSlots = 16
	// jbMaxExtra is how far beyond the target delay the buffer may grow before the
	// oldest frames are skipped to bound latency (sender clock faster than ours).
	jbMaxExtra = 4
	// jbIdleReset: after this many consecutive empty ticks the buffer re-synchronises
	// on the next packet, so a new talk spurt starts at the target delay again.
	jbIdleReset = 25
	// plcMaxFrames: missing frames concealed by repeating the last frame, halving its
	// amplitude each time, before the sender is treated as silent.
	plcMaxFrames = 3

	rawRing        = 32
	parityRing     = 4
	maxAudioPacket = protocol.RTPHeaderSize + protocol.AudioFrameBytes
	maxAudioParity = protocol.RTPHeaderSize + protocol.FECHeaderSize + protocol.AudioFrameBytes
)

type audioFrame [frameSamples]int16

type jbSlot struct {
	valid   bool
	ts      uint32
	samples audioFrame
}

type rawSlot struct {
	valid bool
	seq   uint16
	n     int
	data  [maxAudioPacket]byte
}

type paritySlot struct {
	valid bool
	n     int
	hdr   protocol.FECHeader
	data  [maxAudioParity]byte
}

// pushResult tells the caller what happened to a pushed packet.
type pushResult int

const (
	pushAccepted pushResult = iota
	pushLate
	pushInvalid
)

// jitterBuffer is the small per-sender playout buffer in front of the mixer. The router
// goroutine pushes packets and the room's mixer goroutine pops one frame per tick.
// It also keeps the last packets of the stream so that a parity packet can rebuild
// one lost packet per FEC group.
type jitterBuffer struct {
	mu      sync.Mutex
	delay   int // target delay in frames
	started bool
	cursor  uint32 // RTP timestamp of the next frame to play
	head    int    // ring index of cursor
	idle    int
	slots   [jbSlots]jbSlot

	last     audioFrame
	haveLast bool
	plc      int

	raw        [rawRing]rawSlot
	parity     [parityRing]paritySlot
	nextParity int
	rec        protocol.FECRecovery
	recBuf     [maxAudioPacket]byte

	stats *Stats
}

func newJitterBuffer(delayFrames int, stats *Stats) *jitterBuffer {
	return &jitterBuffer{delay: max(1, min(delayFrames, jbSlots-jbMaxExtra-1)), stats: stats}
}

// push stores an audio packet. pkt is the complete RTP packet, h its parsed header and
// payload its 640-byte L16 payload (both aliasing pkt).
func (jb *jitterBuffer) push(pkt []byte, h *protocol.RTPHeader, payload []byte) pushResult {
	if len(payload) != protocol.AudioFrameBytes || h.CSRCCount != 0 || len(pkt) > maxAudioPacket {
		return pushInvalid
	}
	jb.mu.Lock()
	defer jb.mu.Unlock()
	jb.storeRaw(h.SequenceNumber, pkt)
	res := jb.insert(h.Timestamp, payload)
	for i := range jb.parity {
		if p := &jb.parity[i]; p.valid && p.hdr.Covers(h.SequenceNumber) {
			jb.tryRecover(p)
		}
	}
	return res
}

// pushParity stores a parity packet and rebuilds the group's missing packet if it can.
// It reports false for a malformed parity packet.
func (jb *jitterBuffer) pushParity(pkt []byte) bool {
	if len(pkt) > maxAudioParity || len(pkt) < protocol.RTPHeaderSize+protocol.FECHeaderSize {
		return false
	}
	var hdr protocol.FECHeader
	if _, err := protocol.ParseFEC(pkt[protocol.RTPHeaderSize:], &hdr); err != nil {
		return false
	}
	jb.mu.Lock()
	defer jb.mu.Unlock()
	p := &jb.parity[jb.nextParity]
	jb.nextParity = (jb.nextParity + 1) % parityRing
	p.valid = true
	p.hdr = hdr
	p.n = copy(p.data[:], pkt)
	jb.tryRecover(p)
	return true
}

func (jb *jitterBuffer) storeRaw(seq uint16, pkt []byte) {
	s := &jb.raw[int(seq)%rawRing]
	s.valid = true
	s.seq = seq
	s.n = copy(s.data[:], pkt)
}

func (jb *jitterBuffer) haveRaw(seq uint16) *rawSlot {
	s := &jb.raw[int(seq)%rawRing]
	if s.valid && s.seq == seq {
		return s
	}
	return nil
}

// tryRecover rebuilds the packet missing from p's group when exactly one is missing.
func (jb *jitterBuffer) tryRecover(p *paritySlot) {
	k := int(p.hdr.Count)
	if k > rawRing {
		p.valid = false
		return
	}
	missing, nMissing := uint16(0), 0
	for i := 0; i < k; i++ {
		seq := p.hdr.SNBase + uint16(i)
		if jb.haveRaw(seq) == nil {
			missing = seq
			nMissing++
		}
	}
	if nMissing != 1 {
		if nMissing == 0 {
			p.valid = false
		}
		return
	}
	p.valid = false
	if jb.rec.Start(p.data[:p.n]) != nil {
		return
	}
	for i := 0; i < k; i++ {
		seq := p.hdr.SNBase + uint16(i)
		if s := jb.haveRaw(seq); s != nil {
			if jb.rec.Add(s.data[:s.n]) != nil {
				return
			}
		}
	}
	n, err := jb.rec.Finish(jb.recBuf[:], missing)
	if err != nil {
		return
	}
	var h protocol.RTPHeader
	payload, err := protocol.ParseRTP(jb.recBuf[:n], &h)
	if err != nil || len(payload) != protocol.AudioFrameBytes || h.PayloadType != protocol.PTAudio {
		return
	}
	jb.storeRaw(missing, jb.recBuf[:n])
	if jb.insert(h.Timestamp, payload) == pushAccepted {
		jb.stats.FECRecovered.Add(1)
	}
}

func (jb *jitterBuffer) resync(ts uint32) {
	jb.started = true
	jb.cursor = ts - uint32(jb.delay*frameSamples)
	jb.head = 0
	jb.idle = 0
	jb.haveLast = false
	jb.plc = 0
	for i := range jb.slots {
		jb.slots[i].valid = false
	}
}

// insert places one decoded frame in its slot. Lock must be held.
func (jb *jitterBuffer) insert(ts uint32, payload []byte) pushResult {
	if !jb.started {
		jb.resync(ts)
	}
	d := int32(ts - jb.cursor)
	if d < 0 {
		return pushLate
	}
	if d%frameSamples != 0 || int(d/frameSamples) >= jbSlots {
		jb.resync(ts)
		d = int32(jb.delay * frameSamples)
	}
	idx := int(d / frameSamples)
	if skip := idx - jb.delay - jbMaxExtra; skip > 0 {
		for i := 0; i < skip; i++ {
			jb.slots[jb.head].valid = false
			jb.head = (jb.head + 1) % jbSlots
			jb.cursor += frameSamples
		}
		idx -= skip
	}
	s := &jb.slots[(jb.head+idx)%jbSlots]
	s.valid = true
	s.ts = ts
	for i := range s.samples {
		s.samples[i] = int16(binary.BigEndian.Uint16(payload[2*i:]))
	}
	return pushAccepted
}

// pop returns the frame due at this tick. It reports false when the sender has nothing
// to contribute (not started, or silent longer than the concealment window).
func (jb *jitterBuffer) pop(dst *audioFrame) (ok, concealed bool) {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if !jb.started {
		return false, false
	}
	s := &jb.slots[jb.head]
	got := s.valid && s.ts == jb.cursor
	if got {
		*dst = s.samples
		jb.last = s.samples
		jb.haveLast = true
		jb.plc = 0
		jb.idle = 0
	}
	s.valid = false
	jb.head = (jb.head + 1) % jbSlots
	jb.cursor += frameSamples
	if got {
		return true, false
	}
	jb.idle++
	if jb.idle >= jbIdleReset {
		jb.started = false
		jb.haveLast = false
	}
	if jb.haveLast && jb.plc < plcMaxFrames {
		jb.plc++
		for i, v := range jb.last {
			dst[i] = v >> jb.plc
		}
		jb.stats.AudioConcealed.Add(1)
		return true, true
	}
	return false, false
}
