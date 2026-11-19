package media

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

func constFrame(v int16) *audioFrame {
	var f audioFrame
	for i := range f {
		f[i] = v
	}
	return &f
}

func TestSaturate(t *testing.T) {
	tests := []struct {
		in   int32
		want int16
	}{
		{0, 0},
		{32767, 32767},
		{32768, 32767},
		{1 << 20, 32767},
		{-32768, -32768},
		{-32769, -32768},
		{-1 << 20, -32768},
		{-5, -5},
	}
	for _, tt := range tests {
		if got := saturate(tt.in); got != tt.want {
			t.Errorf("saturate(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestMixMinus(t *testing.T) {
	tests := []struct {
		name    string
		senders []int16 // constant value per active sender
		self    int     // index of the receiver among senders, -1 if not sending
		want    int16
	}{
		{"two others", []int16{1000, 2000, 300}, 2, 3000},
		{"receiver not sending", []int16{1000, 2000}, -1, 3000},
		{"only own voice gives silence", []int16{1234}, 0, 0},
		{"positive saturation", []int16{30000, 30000, 5}, 2, math.MaxInt16},
		{"negative saturation", []int16{-30000, -30000, 0}, 2, math.MinInt16},
		{"own voice removed before saturation", []int16{30000, 30000, 30000}, 0, math.MaxInt16},
		{"cancellation is exact", []int16{32767, -32768, 1}, 2, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var total [frameSamples]int32
			for _, v := range tt.senders {
				accumulate(&total, constFrame(v))
			}
			var own *audioFrame
			if tt.self >= 0 {
				own = constFrame(tt.senders[tt.self])
			}
			var out audioFrame
			mixMinus(&out, &total, own)
			for i, v := range out {
				if v != tt.want {
					t.Fatalf("sample %d = %d, want %d", i, v, tt.want)
				}
			}
		})
	}
}

// audioPacket builds an upstream L16 packet whose samples all equal v.
func audioPacket(seq uint16, ts, ssrc uint32, v int16) []byte {
	h := protocol.RTPHeader{PayloadType: protocol.PTAudio, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}
	buf := make([]byte, protocol.RTPHeaderSize+protocol.AudioFrameBytes)
	n, _ := h.MarshalTo(buf)
	for i := 0; i < frameSamples; i++ {
		binary.BigEndian.PutUint16(buf[n+2*i:], uint16(v))
	}
	return buf
}

func pushPkt(t *testing.T, jb *jitterBuffer, pkt []byte) pushResult {
	t.Helper()
	var h protocol.RTPHeader
	payload, err := protocol.ParseRTP(pkt, &h)
	if err != nil {
		t.Fatal(err)
	}
	return jb.push(pkt, &h, payload)
}

// popValues pops n frames and returns the first sample of each (or -1 for nothing).
func popValues(jb *jitterBuffer, n int) []int {
	out := make([]int, n)
	var f audioFrame
	for i := range out {
		ok, _ := jb.pop(&f)
		if ok {
			out[i] = int(f[0])
		} else {
			out[i] = -1
		}
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestJitterBufferPlayoutDelayAndReorder(t *testing.T) {
	jb := newJitterBuffer(2, &Stats{})
	const ts0 = 1_000_000
	// Frames 1 and 3 arrive before 2 (reordering within the buffer).
	pushPkt(t, jb, audioPacket(1, ts0, 1, 100))
	pushPkt(t, jb, audioPacket(3, ts0+2*frameSamples, 1, 300))
	pushPkt(t, jb, audioPacket(2, ts0+frameSamples, 1, 200))
	// Two ticks of initial delay, then the frames in timestamp order.
	if got := popValues(jb, 5); !equalInts(got, []int{-1, -1, 100, 200, 300}) {
		t.Fatalf("playout = %v", got)
	}
	// A frame for a slot that has already been played is late.
	if res := pushPkt(t, jb, audioPacket(4, ts0+frameSamples, 1, 999)); res != pushLate {
		t.Fatalf("late frame result = %v", res)
	}
}

func TestJitterBufferConcealsThenGoesSilent(t *testing.T) {
	stats := &Stats{}
	jb := newJitterBuffer(1, stats)
	pushPkt(t, jb, audioPacket(1, 0, 1, 800))
	got := popValues(jb, 2+plcMaxFrames+1)
	// delay tick, the frame, then 800>>1, 800>>2, 800>>3, then nothing.
	want := []int{-1, 800, 400, 200, 100, -1}
	if !equalInts(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if stats.AudioConcealed.Load() != plcMaxFrames {
		t.Fatalf("concealed = %d", stats.AudioConcealed.Load())
	}
}

func TestJitterBufferResyncsAfterLongGap(t *testing.T) {
	jb := newJitterBuffer(2, &Stats{})
	pushPkt(t, jb, audioPacket(1, 0, 1, 10))
	popValues(jb, 3)
	// The sender was muted for 10 s: the next talk spurt starts at the target delay
	// again instead of being parked 500 frames in the future.
	pushPkt(t, jb, audioPacket(2, 500*frameSamples, 1, 20))
	got := popValues(jb, 3)
	if got[2] != 20 {
		t.Fatalf("after resync got %v", got)
	}
}

func TestJitterBufferBoundsLatency(t *testing.T) {
	jb := newJitterBuffer(2, &Stats{})
	// A burst of 12 frames arrives at once; only the newest delay+extra frames may stay
	// buffered so playout latency stays bounded.
	for i := 0; i < 12; i++ {
		pushPkt(t, jb, audioPacket(uint16(i), uint32(i*frameSamples), 1, int16(i)))
	}
	got := popValues(jb, 1)
	if got[0] < 12-1-2-jbMaxExtra {
		t.Fatalf("oldest frame still queued: first pop %v", got)
	}
}

func TestJitterBufferRecoversLostPacketFromParity(t *testing.T) {
	stats := &Stats{}
	jb := newJitterBuffer(2, stats)
	p1 := audioPacket(10, 3200, 7, 111)
	p2 := audioPacket(11, 3520, 7, 222)
	var enc protocol.FECEncoder
	_ = enc.Add(p1)
	_ = enc.Add(p2)
	parity := make([]byte, protocol.MaxSendDatagram)
	n, _ := enc.Flush(parity, 7, 12)

	pushPkt(t, jb, p1) // p2 is lost
	if !jb.pushParity(parity[:n]) {
		t.Fatal("parity rejected")
	}
	if stats.FECRecovered.Load() != 1 {
		t.Fatalf("recovered = %d", stats.FECRecovered.Load())
	}
	if got := popValues(jb, 4); !equalInts(got, []int{-1, -1, 111, 222}) {
		t.Fatalf("playout = %v", got)
	}
}

func TestJitterBufferRecoveryWhenParityArrivesFirst(t *testing.T) {
	stats := &Stats{}
	jb := newJitterBuffer(2, stats)
	p1 := audioPacket(65535, 0, 7, 1)
	p2 := audioPacket(0, frameSamples, 7, 2)
	p3 := audioPacket(1, 2*frameSamples, 7, 3)
	var enc protocol.FECEncoder
	for _, p := range [][]byte{p1, p2, p3} {
		_ = enc.Add(p)
	}
	parity := make([]byte, protocol.MaxSendDatagram)
	n, _ := enc.Flush(parity, 7, 2)
	// Reordered: parity first, then two of the three packets (seq wraps at 65535).
	jb.pushParity(parity[:n])
	pushPkt(t, jb, p1)
	pushPkt(t, jb, p3)
	if stats.FECRecovered.Load() != 1 {
		t.Fatalf("recovered = %d", stats.FECRecovered.Load())
	}
	if got := popValues(jb, 5); !equalInts(got, []int{-1, -1, 1, 2, 3}) {
		t.Fatalf("playout = %v", got)
	}
}

func TestJitterBufferRejectsBadAudio(t *testing.T) {
	jb := newJitterBuffer(2, &Stats{})
	pkt := audioPacket(1, 0, 1, 5)
	if res := pushPkt(t, jb, pkt[:len(pkt)-2]); res != pushInvalid {
		t.Fatalf("short frame accepted: %v", res)
	}
	if jb.pushParity([]byte{0x80, 0x7f}) {
		t.Fatal("truncated parity accepted")
	}
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(1000, 1500) // 1000 B/s, 1500 B burst
	now := time.Unix(0, 0)
	if !b.allow(1500, now) {
		t.Fatal("burst not available")
	}
	if b.allow(1, now) {
		t.Fatal("empty bucket admitted a packet")
	}
	if !b.allow(500, now.Add(500*time.Millisecond)) {
		t.Fatal("refill not applied")
	}
	if b.allow(600, now.Add(time.Second)) {
		t.Fatal("refill exceeded rate")
	}
	if !newTokenBucket(0, 0).allow(1<<20, now) {
		t.Fatal("rate 0 must disable the limit")
	}
}
