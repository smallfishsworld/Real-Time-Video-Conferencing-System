package media

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// jitterScenario is one simulated audio stream (bench/jitter_scenarios.json describes
// the fields). bench/jitter_sim.py runs the same scenarios through the client's buffer.
type jitterScenario struct {
	Name        string `json:"name"`
	PeriodUS    int64  `json:"period_us"`
	Frames      int    `json:"frames"`
	DelayUS     int64  `json:"delay_us"`
	JitterUS    int64  `json:"jitter_us"`
	StepFrom    int    `json:"step_from"`
	StepTo      int    `json:"step_to"`
	StepUS      int64  `json:"step_us"`
	BurstFrames int    `json:"burst_frames"`
}

// splitmix64 is the jitter source; the Python simulation uses the same function so
// that both buffers see the same arrival times.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// times returns each frame's send and arrival time in microseconds of the receiver's
// clock.
func (sc *jitterScenario) times(seed uint64) (send, arrive []int64) {
	send = make([]int64, sc.Frames)
	arrive = make([]int64, sc.Frames)
	for k := range sc.Frames {
		send[k] = int64(k) * sc.PeriodUS
		d := sc.DelayUS
		if k >= sc.StepFrom && k < sc.StepTo {
			d += sc.StepUS
		}
		if sc.JitterUS > 0 {
			d += int64(splitmix64(seed+uint64(k)) % uint64(sc.JitterUS))
		}
		arrive[k] = send[k] + d
	}
	for k := 0; k < sc.BurstFrames-1 && k < sc.Frames; k++ {
		arrive[k] = arrive[min(sc.BurstFrames, sc.Frames)-1]
	}
	return send, arrive
}

type latencySummary struct {
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}

func summarize(ms []float64) latencySummary {
	if len(ms) == 0 {
		return latencySummary{-1, -1, -1}
	}
	s := slices.Clone(ms)
	sort.Float64s(s)
	rank := func(p float64) float64 { return s[int(p*float64(len(s)-1)+0.5)] }
	return latencySummary{Median: rank(0.5), P95: rank(0.95), Max: s[len(s)-1]}
}

// jitterSimResult is what one scenario did to the buffer. The active interval runs from
// the first played frame to the sender's last frame: every tick in it should play a
// frame. Latency is pop time minus send time of the played frame.
type jitterSimResult struct {
	Scenario          string         `json:"scenario"`
	Impl              string         `json:"impl"`
	DelayFrames       int            `json:"delay_frames"`
	Frames            int            `json:"frames"`
	ActiveTicks       int            `json:"active_ticks"`
	Played            int            `json:"played"`
	Concealed         int            `json:"concealed"`
	Silent            int            `json:"silent"`
	Late              int            `json:"late"`
	NeverPlayed       int            `json:"never_played"`
	LongestUnplayedMS int            `json:"longest_unplayed_ms"`
	GapsOver60MS      int            `json:"gaps_over_60ms"`
	LatencyLast10sMS  latencySummary `json:"latency_last_10s_ms"`
	LatencyAllMS      latencySummary `json:"latency_all_ms"`
}

// simulateJitter drives a jitterBuffer with sc in simulated time: all packets that
// have arrived by a tick are pushed, then the mixer pops one frame.
func simulateJitter(sc jitterScenario, seed uint64, delayFrames int) jitterSimResult {
	send, arrive := sc.times(seed)
	order := make([]int, sc.Frames)
	for k := range order {
		order[k] = k
	}
	sort.SliceStable(order, func(i, j int) bool { return arrive[order[i]] < arrive[order[j]] })

	stats := &Stats{}
	jb := newJitterBuffer(delayFrames, stats)
	pkt := make([]byte, protocol.RTPHeaderSize+protocol.AudioFrameBytes)
	res := jitterSimResult{Scenario: sc.Name, Impl: "server", DelayFrames: jb.delay, Frames: sc.Frames}
	playedFrame := make([]bool, sc.Frames)
	type tick struct {
		t      int64
		played bool
		conc   bool
		lat    float64
	}
	var ticks []tick
	end := arrive[order[len(order)-1]] + 1_000_000
	ai := 0
	var f audioFrame
	for n := int64(0); ; n++ {
		t := 7000 + n*20000
		if t > end {
			break
		}
		for ; ai < len(order) && arrive[order[ai]] <= t; ai++ {
			k := order[ai]
			h := protocol.RTPHeader{PayloadType: protocol.PTAudio, SequenceNumber: uint16(k),
				Timestamp: uint32(k * frameSamples), SSRC: 1}
			hn, _ := h.MarshalTo(pkt)
			clear(pkt[hn:])
			binary.BigEndian.PutUint16(pkt[hn:], uint16(k&0x7fff))
			binary.BigEndian.PutUint16(pkt[hn+2:], uint16(k>>15))
			var ph protocol.RTPHeader
			payload, err := protocol.ParseRTP(pkt, &ph)
			if err != nil {
				panic(err)
			}
			if jb.push(pkt, &ph, payload) == pushLate {
				res.Late++
			}
		}
		ok, concealed := jb.pop(&f)
		tk := tick{t: t, played: ok && !concealed, conc: ok && concealed}
		if tk.played {
			k := int(f[0]) | int(f[1])<<15
			playedFrame[k] = true
			tk.lat = float64(t-send[k]) / 1000
		}
		ticks = append(ticks, tk)
	}
	first := slices.IndexFunc(ticks, func(tk tick) bool { return tk.played })
	if first < 0 {
		return res
	}
	last := send[sc.Frames-1]
	var all, tail []float64
	run := 0
	for _, tk := range ticks[first:] {
		if tk.t > last {
			break
		}
		res.ActiveTicks++
		switch {
		case tk.played:
			res.Played++
			all = append(all, tk.lat)
			if tk.t > last-10_000_000 {
				tail = append(tail, tk.lat)
			}
			if run > 3 {
				res.GapsOver60MS++
			}
			run = 0
			continue
		case tk.conc:
			res.Concealed++
		default:
			res.Silent++
		}
		run++
		res.LongestUnplayedMS = max(res.LongestUnplayedMS, run*20)
	}
	if run > 3 {
		res.GapsOver60MS++
	}
	for k := range playedFrame {
		if !playedFrame[k] {
			res.NeverPlayed++
		}
	}
	res.LatencyLast10sMS = summarize(tail)
	res.LatencyAllMS = summarize(all)
	return res
}

// TestJitterSimulation writes one JSON line per scenario of bench/jitter_scenarios.json
// (or CONFSERVER_JITTER_SCENARIOS) to CONFSERVER_JITTER_OUT, with the mixer's default
// delay. It is a deterministic simulation, not a pass/fail test; bench/run_jitter.sh
// runs it.
func TestJitterSimulation(t *testing.T) {
	out := os.Getenv("CONFSERVER_JITTER_OUT")
	if out == "" {
		t.Skip("set CONFSERVER_JITTER_OUT to run the jitter-buffer simulation")
	}
	path := os.Getenv("CONFSERVER_JITTER_SCENARIOS")
	if path == "" {
		path = filepath.Join("..", "..", "..", "bench", "jitter_scenarios.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Seed      uint64           `json:"seed"`
		Scenarios []jitterScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, sc := range file.Scenarios {
		line, err := json.Marshal(simulateJitter(sc, file.Seed, DefaultConfig().MixerDelayFrames))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
			t.Fatal(err)
		}
	}
}
