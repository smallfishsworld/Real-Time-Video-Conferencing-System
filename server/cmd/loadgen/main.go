// Command loadgen runs synthetic conference participants against the v2 server or
// against the original Python relay (legacy wire format) and reports frame delivery,
// frame latency and the server's CPU time.
//
// Every participant sends the same JPEG corpus frames (bench/data/frames) at a fixed
// frame rate, plus audio (v2: 20 ms L16 frames; legacy: 1024-sample chunks), and
// receives what the server forwards. Each frame carries a tag in its JPEG COM segment
// with the sender, a sequence number and the send time, so a receiver can measure
// end-to-end latency without clock synchronisation (one process, one clock).
//
// Safety limits (see README "Network usage"): the target must be a loopback address
// unless -allow-remote is given; the number of participants, the offered bitrate and
// the duration are capped.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hard limits that no flag can raise.
const (
	hardMaxParticipants = 512
	hardMaxMbps         = 4000
	hardMaxDuration     = 10 * time.Minute
	legacyMaxID         = 452 // legacy client UDP port 20000 + 100*ID must stay below 65536
)

type config struct {
	Wire         string
	Server       string
	Port         int
	Participants int
	RoomSize     int
	FPS          float64
	FramesDir    string
	Audio        bool
	FECK         int
	AudioFECK    int
	Warmup       time.Duration
	Duration     time.Duration
	Grace        time.Duration
	MaxMbps      float64
	AllowRemote  bool
	LocalBase    string
	ServerPID    int
	ConfBase     int
	Out          string
	Label        string
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.StringVar(&c.Wire, "wire", "v2", "wire format: v2 (Go server) or legacy (original Python relay)")
	fs.StringVar(&c.Server, "server", "127.0.0.1", "server address (an IP literal)")
	fs.IntVar(&c.Port, "port", 0, "control port (default 6100 for v2, 8888 for legacy)")
	fs.IntVar(&c.Participants, "participants", 8, fmt.Sprintf("participants (max %d)", hardMaxParticipants))
	fs.IntVar(&c.RoomSize, "room-size", 4, "participants per room")
	fs.Float64Var(&c.FPS, "fps", 15, "video frames per second per participant")
	fs.StringVar(&c.FramesDir, "frames", "bench/data/frames", "directory of corpus JPEG frames")
	fs.BoolVar(&c.Audio, "audio", true, "send audio as well as video")
	fs.IntVar(&c.FECK, "fec-k", 4, "v2 video FEC group size (0 disables)")
	fs.IntVar(&c.AudioFECK, "audio-fec-k", 2, "v2 audio FEC group size (0 disables)")
	fs.DurationVar(&c.Warmup, "warmup", 5*time.Second, "load before the measurement window")
	fs.DurationVar(&c.Duration, "duration", 20*time.Second, "measurement window")
	fs.DurationVar(&c.Grace, "grace", time.Second, "keep receiving this long after the window for late frames")
	fs.Float64Var(&c.MaxMbps, "max-mbps", 1500, fmt.Sprintf("refuse runs whose offered upstream load exceeds this (max %d)", hardMaxMbps))
	fs.BoolVar(&c.AllowRemote, "allow-remote", false, "allow a non-loopback server address")
	fs.StringVar(&c.LocalBase, "local-base", "127.1.0.0", "legacy: participant i binds base+i (one address per participant)")
	fs.IntVar(&c.ServerPID, "server-pid", 0, "read this process's CPU time from /proc at the window edges")
	fs.IntVar(&c.ConfBase, "conf-base", 1, "legacy: first conference ID")
	fs.StringVar(&c.Out, "out", "", "write the result JSON here (default stdout)")
	fs.StringVar(&c.Label, "label", "", "free-form label stored in the result")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.Port == 0 {
		c.Port = 6100
		if c.Wire == "legacy" {
			c.Port = 8888
		}
	}
	return c, nil
}

// validate enforces the safety limits before any packet is sent.
func (c *config) validate(avgFrameBytes float64) error {
	if c.Wire != "v2" && c.Wire != "legacy" {
		return fmt.Errorf("unknown -wire %q", c.Wire)
	}
	addr, err := netip.ParseAddr(c.Server)
	if err != nil {
		return fmt.Errorf("-server must be an IP address: %w", err)
	}
	if !addr.IsLoopback() && !c.AllowRemote {
		return fmt.Errorf("refusing non-loopback target %s without -allow-remote", addr)
	}
	if c.Participants < 1 || c.Participants > hardMaxParticipants {
		return fmt.Errorf("-participants must be 1..%d", hardMaxParticipants)
	}
	if c.Wire == "legacy" && c.Participants > legacyMaxID {
		return fmt.Errorf("the legacy port scheme supports at most %d participants", legacyMaxID)
	}
	if c.RoomSize < 2 {
		return errors.New("-room-size must be at least 2")
	}
	if c.FPS <= 0 || c.FPS > 60 {
		return errors.New("-fps must be in (0, 60]")
	}
	if c.Warmup+c.Duration+c.Grace > hardMaxDuration {
		return fmt.Errorf("run longer than %s", hardMaxDuration)
	}
	if c.MaxMbps > hardMaxMbps {
		return fmt.Errorf("-max-mbps above the hard cap of %d", hardMaxMbps)
	}
	if mbps := c.offeredMbps(avgFrameBytes); mbps > c.MaxMbps {
		return fmt.Errorf("offered load %.0f Mbit/s exceeds -max-mbps %.0f", mbps, c.MaxMbps)
	}
	return nil
}

// offeredMbps is the upstream media bitrate of all participants (payload only).
func (c *config) offeredMbps(avgFrameBytes float64) float64 {
	per := avgFrameBytes * 8 * c.FPS
	if c.Audio {
		per += 16000 * 16
	}
	return per * float64(c.Participants) / 1e6
}

// The frame tag lives in the JPEG COM segment that the corpus frames carry right
// after SOI: FF D8 FF FE <len=34> <32 bytes>.
const (
	comOffset = 6
	comLen    = 32
	tagMagic  = 0x4C47454E // "LGEN"
)

type tag struct {
	Sender uint32
	Seq    uint32
	SentNS int64
}

func writeTag(frame []byte, t tag) {
	b := frame[comOffset : comOffset+comLen]
	binary.BigEndian.PutUint32(b[0:], tagMagic)
	binary.BigEndian.PutUint32(b[4:], t.Sender)
	binary.BigEndian.PutUint32(b[8:], t.Seq)
	binary.BigEndian.PutUint64(b[12:], uint64(t.SentNS))
}

func readTag(jpeg []byte) (tag, bool) {
	if len(jpeg) < comOffset+comLen || jpeg[0] != 0xFF || jpeg[1] != 0xD8 || jpeg[2] != 0xFF || jpeg[3] != 0xFE {
		return tag{}, false
	}
	b := jpeg[comOffset : comOffset+comLen]
	if binary.BigEndian.Uint32(b[0:]) != tagMagic {
		return tag{}, false
	}
	return tag{
		Sender: binary.BigEndian.Uint32(b[4:]),
		Seq:    binary.BigEndian.Uint32(b[8:]),
		SentNS: int64(binary.BigEndian.Uint64(b[12:])),
	}, true
}

func loadFrames(dir string) ([][]byte, error) {
	files, err := filepath.Glob(filepath.Join(dir, "frame_*.jpg"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no frame_*.jpg in %s (run bench/gen_frames.py)", dir)
	}
	frames := make([][]byte, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if _, ok := readTag(b); !ok && (len(b) < comOffset+comLen || b[2] != 0xFF || b[3] != 0xFE) {
			return nil, fmt.Errorf("%s has no COM segment after SOI", f)
		}
		frames = append(frames, b)
	}
	return frames, nil
}

// clock is the run's monotonic time base.
type clock struct{ start time.Time }

func (c clock) now() int64 { return int64(time.Since(c.start)) }

// recorder collects what receivers observe; each receiver goroutine owns one.
type recorder struct {
	latMS         []float32
	framesInWin   int64
	audioInWin    int64
	videoPackets  int64
	otherPackets  int64
	badTags       int64
	incomplete    int64
	videoBytes    int64
	audioBytes    int64
	readErrors    int64
	windowStartNS int64
	windowEndNS   int64
}

func (r *recorder) inWindow(ns int64) bool { return ns >= r.windowStartNS && ns < r.windowEndNS }

func (r *recorder) frame(t tag, now int64) {
	if r.inWindow(t.SentNS) {
		r.framesInWin++
		r.latMS = append(r.latMS, float32(now-t.SentNS)/1e6)
	}
}

// senderStats counts what a sender put on the wire.
type senderStats struct {
	framesInWin atomic.Int64
	audioInWin  atomic.Int64
	packets     atomic.Int64
	bytes       atomic.Int64
	sendErrors  atomic.Int64
	framesTotal atomic.Int64
	audioTotal  atomic.Int64
}

type participant interface {
	sendVideo(frame []byte, t tag) error
	sendAudio(seq uint32) error
	receive(ctx context.Context, rec *recorder)
	// controlLost reports whether the participant's control connection has ended, in
	// which case the server has removed it from its room.
	controlLost() bool
	close()
}

type result struct {
	Label                string             `json:"label,omitempty"`
	Wire                 string             `json:"wire"`
	Participants         int                `json:"participants"`
	Rooms                int                `json:"rooms"`
	RoomSize             int                `json:"room_size"`
	FPS                  float64            `json:"fps"`
	Audio                bool               `json:"audio"`
	FECK                 int                `json:"fec_k"`
	AudioFECK            int                `json:"audio_fec_k"`
	WindowSeconds        float64            `json:"window_seconds"`
	AvgFrameBytes        float64            `json:"avg_frame_bytes"`
	OfferedUpMbps        float64            `json:"offered_upstream_mbps"`
	FramesSent           int64              `json:"frames_sent_in_window"`
	FramesExpected       int64              `json:"frames_expected"`
	FramesReceived       int64              `json:"frames_received"`
	DeliveryRatio        float64            `json:"delivery_ratio"`
	LatencyMS            map[string]float64 `json:"latency_ms"`
	AudioExpected        int64              `json:"audio_expected"`
	AudioReceived        int64              `json:"audio_received"`
	AudioDeliveryRatio   float64            `json:"audio_delivery_ratio"`
	SendErrors           int64              `json:"send_errors"`
	PacketsSent          int64              `json:"packets_sent"`
	BytesSent            int64              `json:"bytes_sent"`
	VideoPacketsReceived int64              `json:"video_packets_received"`
	IncompleteFrames     int64              `json:"incomplete_frames"`
	ServerCPUSeconds     *float64           `json:"server_cpu_seconds,omitempty"`
	ServerCPUPerPartMin  *float64           `json:"server_cpu_seconds_per_participant_minute,omitempty"`
	ServerCPUUtil        *float64           `json:"server_cpu_utilisation,omitempty"`
	LoadgenCPUSeconds    float64            `json:"loadgen_cpu_seconds"`
	LoadgenCPUUtil       float64            `json:"loadgen_cpu_utilisation"`
	LoadgenGOMAXPROCS    int                `json:"loadgen_gomaxprocs"`
	ControlLost          int                `json:"control_connections_lost"`
	StartedUTC           string             `json:"started_utc"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	frames, err := loadFrames(cfg.FramesDir)
	if err != nil {
		return err
	}
	var total float64
	for _, f := range frames {
		total += float64(len(f))
	}
	avg := total / float64(len(frames))
	if err := cfg.validate(avg); err != nil {
		return err
	}
	clk := clock{start: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := netip.MustParseAddr(cfg.Server)
	parts := make([]participant, cfg.Participants)
	stats := make([]*senderStats, cfg.Participants)
	for i := range stats {
		stats[i] = &senderStats{}
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer setupCancel()
	switch cfg.Wire {
	case "v2":
		err = setupV2(setupCtx, cfg, clk, server, parts, stats)
	case "legacy":
		err = setupLegacy(setupCtx, cfg, clk, server, parts, stats)
	}
	if err != nil {
		for _, p := range parts {
			if p != nil {
				p.close()
			}
		}
		return fmt.Errorf("setup: %w", err)
	}
	defer func() {
		for _, p := range parts {
			p.close()
		}
	}()

	// The window starts after the warmup, measured from now.
	t0 := clk.now()
	winStart := t0 + int64(cfg.Warmup)
	winEnd := winStart + int64(cfg.Duration)
	stopSend := winEnd + int64(cfg.Grace)

	recs := make([]*recorder, len(parts))
	var rwg sync.WaitGroup
	recvCtx, recvCancel := context.WithCancel(ctx)
	for i, p := range parts {
		recs[i] = &recorder{windowStartNS: winStart, windowEndNS: winEnd}
		rwg.Add(1)
		go func(p participant, r *recorder) {
			defer rwg.Done()
			p.receive(recvCtx, r)
		}(p, recs[i])
	}

	var swg sync.WaitGroup
	period := time.Duration(float64(time.Second) / cfg.FPS)
	audioPeriod := 20 * time.Millisecond
	if cfg.Wire == "legacy" {
		audioPeriod = 64 * time.Millisecond // CHUNK = 1024 samples at 16 kHz
	}
	for i, p := range parts {
		swg.Add(1)
		go func(i int, p participant, st *senderStats) {
			defer swg.Done()
			sendLoop(clk, cfg, frames, i, p, st, period, audioPeriod, winStart, winEnd, stopSend)
		}(i, p, stats[i])
	}

	var cpu0, cpu1 float64
	var haveCPU bool
	sleepUntil(clk, winStart)
	if cfg.ServerPID > 0 {
		if v, err := procCPUSeconds(cfg.ServerPID); err == nil {
			cpu0, haveCPU = v, true
		}
	}
	selfWin0 := selfCPUSeconds()
	sleepUntil(clk, winEnd)
	if haveCPU {
		if v, err := procCPUSeconds(cfg.ServerPID); err == nil {
			cpu1 = v
		} else {
			haveCPU = false
		}
	}
	selfWin1 := selfCPUSeconds()
	lost := 0
	for _, p := range parts {
		if p.controlLost() {
			lost++
		}
	}
	swg.Wait()
	recvCancel()
	rwg.Wait()

	res := result{
		Label:             cfg.Label,
		Wire:              cfg.Wire,
		Participants:      cfg.Participants,
		Rooms:             (cfg.Participants + cfg.RoomSize - 1) / cfg.RoomSize,
		RoomSize:          cfg.RoomSize,
		FPS:               cfg.FPS,
		Audio:             cfg.Audio,
		WindowSeconds:     cfg.Duration.Seconds(),
		AvgFrameBytes:     math.Round(avg),
		OfferedUpMbps:     cfg.offeredMbps(avg),
		LoadgenGOMAXPROCS: runtime.GOMAXPROCS(0),
		ControlLost:       lost,
		StartedUTC:        clk.start.UTC().Format(time.RFC3339),
	}
	if cfg.Wire == "v2" {
		res.FECK, res.AudioFECK = cfg.FECK, cfg.AudioFECK
	}
	// Each frame a sender sends in the window should reach every other member.
	var lat []float64
	for i := range parts {
		members := roomMembers(cfg, i)
		res.FramesSent += stats[i].framesInWin.Load()
		res.FramesExpected += stats[i].framesInWin.Load() * int64(members-1)
		res.PacketsSent += stats[i].packets.Load()
		res.BytesSent += stats[i].bytes.Load()
		res.SendErrors += stats[i].sendErrors.Load()
		r := recs[i]
		res.FramesReceived += r.framesInWin
		res.AudioReceived += r.audioInWin
		res.VideoPacketsReceived += r.videoPackets
		res.IncompleteFrames += r.incomplete
		for _, v := range r.latMS {
			lat = append(lat, float64(v))
		}
		if cfg.Audio && members > 1 {
			if cfg.Wire == "v2" {
				// One mixed stream per receiver: a frame every 20 ms while others talk.
				res.AudioExpected += int64(cfg.Duration / audioPeriod)
			} else {
				// Every chunk a sender sends in the window goes to each other member.
				// Receivers count chunks by arrival time, so a stream can be off by one
				// chunk at each window edge.
				res.AudioExpected += stats[i].audioInWin.Load() * int64(members-1)
			}
		}
	}
	if res.FramesExpected > 0 {
		res.DeliveryRatio = float64(res.FramesReceived) / float64(res.FramesExpected)
	}
	if res.AudioExpected > 0 {
		res.AudioDeliveryRatio = float64(res.AudioReceived) / float64(res.AudioExpected)
	}
	res.LatencyMS = percentiles(lat)
	if haveCPU {
		d := cpu1 - cpu0
		perPM := d / (float64(cfg.Participants) * cfg.Duration.Minutes())
		util := d / cfg.Duration.Seconds()
		res.ServerCPUSeconds, res.ServerCPUPerPartMin, res.ServerCPUUtil = &d, &perPM, &util
	}
	res.LoadgenCPUSeconds = selfWin1 - selfWin0
	res.LoadgenCPUUtil = res.LoadgenCPUSeconds / cfg.Duration.Seconds()

	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if cfg.Out == "" {
		_, err = os.Stdout.Write(out)
		return err
	}
	return os.WriteFile(cfg.Out, out, 0o644)
}

// roomMembers is the size of participant i's room (the last room may be smaller).
func roomMembers(cfg config, i int) int {
	room := i / cfg.RoomSize
	first := room * cfg.RoomSize
	return min(cfg.RoomSize, cfg.Participants-first)
}

func sendLoop(clk clock, cfg config, frames [][]byte, i int, p participant, st *senderStats,
	period, audioPeriod time.Duration, winStart, winEnd, stop int64) {
	// Spread participants' phases over one frame period, so that the offered load is
	// smooth rather than synchronised bursts.
	phase := int64(period) * int64(i) / int64(cfg.Participants)
	nextVideo := clk.now() + phase
	nextAudio := clk.now() + int64(audioPeriod)*int64(i)/int64(cfg.Participants)
	buf := make([]byte, 0, 64<<10)
	var seq, aseq uint32
	for {
		next := nextVideo
		if cfg.Audio && nextAudio < next {
			next = nextAudio
		}
		if next >= stop {
			return
		}
		sleepUntil(clk, next)
		now := clk.now()
		if now >= nextVideo {
			src := frames[int(seq)%len(frames)]
			buf = append(buf[:0], src...)
			t := tag{Sender: uint32(i), Seq: seq, SentNS: now}
			writeTag(buf, t)
			if err := p.sendVideo(buf, t); err != nil {
				st.sendErrors.Add(1)
			}
			if now >= winStart && now < winEnd {
				st.framesInWin.Add(1)
			}
			st.framesTotal.Add(1)
			seq++
			nextVideo += int64(period)
			if now-nextVideo > int64(time.Second) { // fell far behind: do not burst
				nextVideo = now + int64(period)
			}
		}
		if cfg.Audio && now >= nextAudio {
			if err := p.sendAudio(aseq); err != nil {
				st.sendErrors.Add(1)
			}
			if now >= winStart && now < winEnd {
				st.audioInWin.Add(1)
			}
			st.audioTotal.Add(1)
			aseq++
			nextAudio += int64(audioPeriod)
			if now-nextAudio > int64(time.Second) {
				nextAudio = now + int64(audioPeriod)
			}
		}
	}
}

func sleepUntil(clk clock, ns int64) {
	if d := time.Duration(ns - clk.now()); d > 0 {
		time.Sleep(d)
	}
}

func percentiles(v []float64) map[string]float64 {
	if len(v) == 0 {
		return map[string]float64{}
	}
	sort.Float64s(v)
	q := func(p float64) float64 {
		idx := p / 100 * float64(len(v)-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		return v[lo] + (v[hi]-v[lo])*(idx-float64(lo))
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return map[string]float64{
		"mean": sum / float64(len(v)),
		"p50":  q(50),
		"p90":  q(90),
		"p99":  q(99),
		"max":  v[len(v)-1],
	}
}

// procCPUSeconds returns utime+stime of a process from /proc/<pid>/stat.
func procCPUSeconds(pid int) (float64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseStatCPU(string(b))
}

func selfCPUSeconds() float64 {
	v, err := procCPUSeconds(os.Getpid())
	if err != nil {
		return 0
	}
	return v
}

// clkTck is USER_HZ, which is 100 on every Linux architecture Go supports.
const clkTck = 100

func parseStatCPU(stat string) (float64, error) {
	// The command name (field 2) may contain spaces; it ends at the last ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(stat[end+1:])
	// fields[0] is field 3 (state); utime and stime are fields 14 and 15.
	if len(fields) < 13 {
		return 0, errors.New("malformed /proc stat")
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, errors.New("malformed /proc stat")
	}
	return float64(utime+stime) / clkTck, nil
}
