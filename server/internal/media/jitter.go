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
	// oldest frames are skipped at once (a burst of packets).
	jbMaxExtra = 4
	// jbIdleReset: after this many consecutive empty ticks the buffer re-synchronises
	// on the next packet, so a new talk spurt starts at the target delay again.
	jbIdleReset = 25
	// jbLateResync: when this many packets in a row arrive after their playout time,
	// the sender's delay has grown, and the buffer re-anchors on the newest of them
	// (keeping them) instead of dropping packets until jbIdleReset empty ticks.
	jbLateResync = 3
	// jbWindow: every jbWindow ticks (500 ms) the buffer compares its level, the
	// frames it holds beyond the playout cursor, with the target delay. A window in
	// which the level never fell to the target drops one frame (the sender's clock
	// is faster than ours, or its delay fell); a window in which it never reached
	// the target plays one concealed frame without advancing (a slower clock). That
	// keeps the latency at the target and the buffer from running dry.
	jbWindow = 25
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

	newest         uint32               // highest timestamp stored since the last resync
	late           [jbLateResync]jbSlot // the current run of late frames
	lateN          int
	win            int // ticks in the current level window
	winMin, winMax int

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

// MaxMixerDelayFrames is the largest jitter-buffer delay the mixer supports, in 20 ms
// frames; Config.MixerDelayFrames is clamped to 1..MaxMixerDelayFrames.
const MaxMixerDelayFrames = jbSlots - jbMaxExtra - 1

func newJitterBuffer(delayFrames int, stats *Stats) *jitterBuffer {
	return &jitterBuffer{delay: max(1, min(delayFrames, MaxMixerDelayFrames)), stats: stats}
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

// resync anchors the playout cursor delay frames behind ts and empties the buffer.
func (jb *jitterBuffer) resync(ts uint32) {
	jb.started = true
	jb.cursor = ts - uint32(jb.delay*frameSamples)
	jb.newest = ts
	jb.head = 0
	jb.idle = 0
	jb.haveLast = false
	jb.plc = 0
	jb.lateN = 0
	jb.resetWindow()
	for i := range jb.slots {
		jb.slots[i].valid = false
	}
}

func (jb *jitterBuffer) resetWindow() {
	jb.win, jb.winMin, jb.winMax = 0, jbSlots, -jbSlots
}

func decodeFrame(dst *audioFrame, payload []byte) {
	for i := range dst {
		dst[i] = int16(binary.BigEndian.Uint16(payload[2*i:]))
	}
}

// insert places one decoded frame in its slot. Lock must be held.
func (jb *jitterBuffer) insert(ts uint32, payload []byte) pushResult {
	if !jb.started {
		jb.resync(ts)
	}
	d := int32(ts - jb.cursor)
	if d < 0 {
		return jb.insertLate(ts, payload)
	}
	jb.lateN = 0
	if d%frameSamples != 0 || int(d/frameSamples) >= jbSlots {
		jb.resync(ts)
		d = int32(jb.delay * frameSamples)
	}
	if int32(ts-jb.newest) > 0 {
		jb.newest = ts
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
	decodeFrame(&s.samples, payload)
	return pushAccepted
}

// insertLate handles a frame whose playout time has passed. After jbLateResync of
// them in a row it re-anchors the cursor delay frames behind the newest and stores
// the run, so the stream continues at its new delay. Lock must be held.
func (jb *jitterBuffer) insertLate(ts uint32, payload []byte) pushResult {
	l := &jb.late[jb.lateN%jbLateResync]
	l.valid, l.ts = true, ts
	decodeFrame(&l.samples, payload)
	jb.lateN++
	if jb.lateN < jbLateResync {
		return pushLate
	}
	haveLast := jb.haveLast
	jb.resync(ts)
	jb.haveLast = haveLast // conceal across the jump as across a loss
	for i := range jb.late {
		l := &jb.late[i]
		d := int32(l.ts - jb.cursor)
		if l.valid && d >= 0 && d%frameSamples == 0 && int(d/frameSamples) < jbSlots {
			s := &jb.slots[(jb.head+int(d/frameSamples))%jbSlots]
			s.valid, s.ts, s.samples = true, l.ts, l.samples
		}
		l.valid = false
	}
	jb.stats.AudioReanchored.Add(1)
	return pushAccepted
}

// level tracks the frames held beyond the cursor and, once per window, says whether
// to drop a frame (+1), repeat one (-1) or do nothing (0). Lock must be held.
func (jb *jitterBuffer) level() int {
	lv := int(int32(jb.newest-jb.cursor)) / frameSamples
	jb.winMin = min(jb.winMin, lv)
	jb.winMax = max(jb.winMax, lv)
	if jb.win++; jb.win < jbWindow {
		return 0
	}
	lo, hi := jb.winMin, jb.winMax
	jb.resetWindow()
	switch {
	case lo > jb.delay:
		return 1
	case hi < jb.delay:
		return -1
	}
	return 0
}

// pop returns the frame due at this tick. It reports false when the sender has nothing
// to contribute (not started, or silent longer than the concealment window).
func (jb *jitterBuffer) pop(dst *audioFrame) (ok, concealed bool) {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if !jb.started {
		return false, false
	}
	switch jb.level() {
	case 1: // drop the frame due now and play the next one
		jb.slots[jb.head].valid = false
		jb.head = (jb.head + 1) % jbSlots
		jb.cursor += frameSamples
		jb.stats.AudioDrained.Add(1)
	case -1: // stretch: conceal one frame and play the due one at the next tick
		if s := &jb.slots[jb.head]; jb.haveLast && s.valid && s.ts == jb.cursor {
			for i, v := range jb.last {
				dst[i] = v >> 1
			}
			jb.stats.AudioStretched.Add(1)
			return true, true
		}
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
