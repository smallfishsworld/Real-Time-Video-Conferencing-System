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
// Safety limits (see README "Network usage limits"): the target must be a loopback
// address unless -allow-remote is given. Before a run starts, the load generator
// estimates the traffic it causes on the wire in both directions (its own sends with
// FEC parity and every header down to IP and UDP, the server's fan-out to the other
// members of each room, and the mixed audio) and refuses the run if the total
// bitrate or packet rate is over the limit. A non-loopback target gets much lower
// defaults and hard caps, and a room size of at most 16. While it runs, a watchdog
// stops the run if what it actually sends is over the estimate.
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

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Hard limits that no flag can raise.
const (
	hardMaxParticipants = 512
	hardMaxDuration     = 10 * time.Minute
	legacyMaxID         = 452 // legacy client UDP port 20000 + 100*ID must stay below 65536

	// Total wire traffic (both directions) on loopback, where nothing leaves the host.
	defaultLoopbackMbps = 2000
	hardLoopbackMbps    = 16000
	defaultLoopbackPPS  = 400_000
	hardLoopbackPPS     = 2_000_000

	// A non-loopback target is a real network that others share.
	defaultRemoteMbps         = 25 // two participants at the default 15 fps
	hardRemoteMbps            = 100
	defaultRemotePPS          = 2500
	hardRemotePPS             = 5000
	hardRemoteRoomSize        = 16
	hardRemoteParticipants    = 64
	remoteMTU                 = 1500 // for counting the IP fragments of large legacy datagrams
	ipUDPHeader               = 28   // IPv4 + UDP header bytes per datagram
	ipHeader                  = 20
	watchdogSlack             = 1.25 // measured send rate allowed over the estimate
	mixerAudioFECK            = 2    // the server's default -audio-fec-k
	legacyAudioChunkBytes     = 2048 // 1024 samples of 16-bit audio
	legacyAudioChunksPerSec   = 16000.0 / 1024
	v2AudioFramesPerSec       = 50
	v2AudioPayloadBytes       = 640
	v2MaxCSRC                 = 15
	pickleWorstCaseOverheadID = 300 // an ID above 255 takes the longer pickle encoding
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
	MaxPPS       float64
	AllowRemote  bool
	LocalBase    string
	ServerPID    int
	ServerCore   int
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
	fs.Float64Var(&c.MaxMbps, "max-mbps", 0, fmt.Sprintf(
		"refuse runs whose estimated total wire traffic, both directions with all headers, exceeds this many Mbit/s "+
			"(default %d on loopback, %d otherwise; at most %d and %d)",
		defaultLoopbackMbps, defaultRemoteMbps, hardLoopbackMbps, hardRemoteMbps))
	fs.Float64Var(&c.MaxPPS, "max-pps", 0, fmt.Sprintf(
		"refuse runs whose estimated total packet rate exceeds this many packets/s "+
			"(default %d on loopback, %d otherwise; at most %d and %d)",
		defaultLoopbackPPS, defaultRemotePPS, hardLoopbackPPS, hardRemotePPS))
	fs.BoolVar(&c.AllowRemote, "allow-remote", false, fmt.Sprintf(
		"allow a non-loopback server address (lower limits apply: room size <= %d, <= %d participants)",
		hardRemoteRoomSize, hardRemoteParticipants))
	fs.StringVar(&c.LocalBase, "local-base", "127.1.0.0", "legacy: participant i binds base+i (one address per participant)")
	fs.IntVar(&c.ServerPID, "server-pid", 0, "read this process's CPU time from /proc at the window edges")
	fs.IntVar(&c.ServerCore, "server-core", -1, "read this CPU's busy and softirq time from /proc/stat at the window edges "+
		"(the core the server is pinned to; softirq work there is charged to whatever task it interrupted)")
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

// remote reports whether the target is not a loopback address (validate has checked
// that it parses).
func (c *config) remote() bool {
	addr, err := netip.ParseAddr(c.Server)
	return err != nil || !addr.IsLoopback()
}

// limits returns the bitrate (Mbit/s) and packet-rate caps for the target: the flags'
// values, or the target's defaults when they are 0.
func (c *config) limits() (mbps, pps float64) {
	mbps, pps = c.MaxMbps, c.MaxPPS
	if mbps == 0 {
		mbps = defaultLoopbackMbps
		if c.remote() {
			mbps = defaultRemoteMbps
		}
	}
	if pps == 0 {
		pps = defaultLoopbackPPS
		if c.remote() {
			pps = defaultRemotePPS
		}
	}
	return mbps, pps
}

// validate enforces the safety limits before any packet is sent. maxFrameBytes is the
// largest corpus frame, so the traffic estimate it checks is an upper bound.
func (c *config) validate(maxFrameBytes int) error {
	if c.Wire != "v2" && c.Wire != "legacy" {
		return fmt.Errorf("unknown -wire %q", c.Wire)
	}
	addr, err := netip.ParseAddr(c.Server)
	if err != nil {
		return fmt.Errorf("-server must be an IP address: %w", err)
	}
	remote := !addr.IsLoopback()
	if remote && !c.AllowRemote {
		return fmt.Errorf("refusing non-loopback target %s without -allow-remote", addr)
	}
	if c.Participants < 1 || c.Participants > hardMaxParticipants {
		return fmt.Errorf("-participants must be 1..%d", hardMaxParticipants)
	}
	if c.Wire == "legacy" && c.Participants > legacyMaxID {
		return fmt.Errorf("the legacy port scheme supports at most %d participants", legacyMaxID)
	}
	if c.RoomSize < 2 || c.RoomSize > hardMaxParticipants {
		return fmt.Errorf("-room-size must be 2..%d", hardMaxParticipants)
	}
	if remote && c.RoomSize > hardRemoteRoomSize {
		return fmt.Errorf("-room-size above %d is refused for a non-loopback target", hardRemoteRoomSize)
	}
	if remote && c.Participants > hardRemoteParticipants {
		return fmt.Errorf("more than %d participants are refused for a non-loopback target", hardRemoteParticipants)
	}
	// Written so that NaN fails every check.
	if !(c.FPS >= 0.1 && c.FPS <= 60) {
		return errors.New("-fps must be in 0.1..60")
	}
	if c.FECK < 0 || c.FECK > protocol.MaxFECGroup || c.AudioFECK < 0 || c.AudioFECK > protocol.MaxFECGroup {
		return fmt.Errorf("-fec-k and -audio-fec-k must be 0..%d", protocol.MaxFECGroup)
	}
	for _, d := range []struct {
		name string
		v    time.Duration
		min  time.Duration
	}{{"-warmup", c.Warmup, 0}, {"-duration", c.Duration, time.Second}, {"-grace", c.Grace, 0}} {
		if d.v < d.min || d.v > hardMaxDuration {
			return fmt.Errorf("%s must be %s..%s", d.name, d.min, hardMaxDuration)
		}
	}
	if c.Warmup+c.Duration+c.Grace > hardMaxDuration { // each is bounded: no overflow
		return fmt.Errorf("run longer than %s", hardMaxDuration)
	}
	hardMbps, hardPPS := float64(hardLoopbackMbps), float64(hardLoopbackPPS)
	if remote {
		hardMbps, hardPPS = hardRemoteMbps, hardRemotePPS
	}
	if !(c.MaxMbps >= 0 && c.MaxMbps <= hardMbps) {
		return fmt.Errorf("-max-mbps must be 0 (the default) up to the hard cap of %.0f for this target", hardMbps)
	}
	if !(c.MaxPPS >= 0 && c.MaxPPS <= hardPPS) {
		return fmt.Errorf("-max-pps must be 0 (the default) up to the hard cap of %.0f for this target", hardPPS)
	}
	maxMbps, maxPPS := c.limits()
	est := c.estimate(maxFrameBytes)
	if est.TotalMbps > maxMbps {
		return fmt.Errorf("estimated traffic %.0f Mbit/s (%.0f up, %.0f down, all headers) exceeds -max-mbps %.0f",
			est.TotalMbps, est.UpMbps, est.DownMbps, maxMbps)
	}
	if est.TotalPPS > maxPPS {
		return fmt.Errorf("estimated %.0f packets/s (%.0f up, %.0f down) exceeds -max-pps %.0f",
			est.TotalPPS, est.UpPPS, est.DownPPS, maxPPS)
	}
	return nil
}

// load is an estimate of the traffic a run causes on the wire, with every header down
// to IP and UDP: what the load generator sends (up) and what the server sends to it
// (down). On loopback both cross the same interface.
type load struct {
	UpMbps    float64 `json:"upstream_mbps"`
	DownMbps  float64 `json:"downstream_mbps"`
	TotalMbps float64 `json:"total_mbps"`
	UpPPS     float64 `json:"upstream_pps"`
	DownPPS   float64 `json:"downstream_pps"`
	TotalPPS  float64 `json:"total_pps"`
}

// datagram returns the IP packets and wire bytes of one UDP datagram with n payload
// bytes. On a real network a datagram larger than the MTU leaves as IP fragments.
func (c *config) datagram(n int) (pkts, bytes float64) {
	if c.remote() && n+8 > remoteMTU-ipHeader {
		frags := (n + 8 + remoteMTU - ipHeader - 1) / (remoteMTU - ipHeader)
		return float64(frags), float64(n + 8 + ipHeader*frags)
	}
	return 1, float64(n + ipUDPHeader)
}

// v2Frame returns the datagram sizes the v2 load generator sends for one frame of n
// bytes: its fragments and, with -fec-k, one parity packet per group (the same layout
// as the client's VideoPacketizer.wire_size).
func (c *config) v2Frame(n int) []int {
	count := protocol.FragmentCount(n)
	out := make([]int, 0, count+count/max(c.FECK, 1)+1)
	for i := 0; i < count; i++ {
		frag := min(protocol.MaxFragmentPayload, n-i*protocol.MaxFragmentPayload)
		out = append(out, protocol.RTPHeaderSize+protocol.VideoHeaderSize+frag)
		if c.FECK > 0 && ((i+1)%c.FECK == 0 || i == count-1) {
			body := 0 // the longest protected packet of the group, without its RTP header
			for j := i - i%c.FECK; j <= i; j++ {
				body = max(body, protocol.VideoHeaderSize+min(protocol.MaxFragmentPayload, n-j*protocol.MaxFragmentPayload))
			}
			out = append(out, protocol.RTPHeaderSize+protocol.FECHeaderSize+body)
		}
	}
	return out
}

// pickleSize is the size of the legacy datagram pickle.dumps((id, kind, data)) with
// len(data) == n, for an ID above 255 (the larger encoding).
func pickleSize(n int) int {
	body := 3 + 2 + 2 // BININT2 id, BININT1 kind, SHORT_BINBYTES
	if n >= 256 {
		body = 3 + 2 + 5 // BINBYTES
	}
	return 3 + 8 + body + n + 4
}

// estimate computes the traffic of the run for frames of frameBytes bytes.
func (c *config) estimate(frameBytes int) load {
	var up, upP, down, downP float64 // bytes and packets per second
	add := func(n int, perSec, copies float64) {
		p, b := c.datagram(n)
		up += b * perSec
		upP += p * perSec
		down += b * perSec * copies
		downP += p * perSec * copies
	}
	for i := 0; i < c.Participants; i++ {
		others := float64(roomMembers(*c, i) - 1)
		if c.Wire == "v2" {
			for _, n := range c.v2Frame(frameBytes) {
				add(n, c.FPS, others) // the server forwards every packet to each other member
			}
			if !c.Audio {
				continue
			}
			add(protocol.RTPHeaderSize+v2AudioPayloadBytes, v2AudioFramesPerSec, 0)
			if c.AudioFECK > 0 {
				add(protocol.RTPHeaderSize+protocol.FECHeaderSize+v2AudioPayloadBytes,
					float64(v2AudioFramesPerSec)/float64(c.AudioFECK), 0)
			}
			if others > 0 { // one mixed stream back, with CSRCs and the mixer's parity
				mixed := protocol.RTPHeaderSize + 4*min(int(others), v2MaxCSRC) + v2AudioPayloadBytes
				p, b := c.datagram(mixed)
				down += b * v2AudioFramesPerSec
				downP += p * v2AudioFramesPerSec
				p, b = c.datagram(mixed + protocol.FECHeaderSize)
				down += b * v2AudioFramesPerSec / mixerAudioFECK
				downP += p * v2AudioFramesPerSec / mixerAudioFECK
			}
		} else {
			add(pickleSize(frameBytes), c.FPS, others) // the relay sends each datagram to every other member
			if c.Audio {
				add(pickleSize(legacyAudioChunkBytes), legacyAudioChunksPerSec, others)
			}
		}
	}
	l := load{UpMbps: up * 8 / 1e6, DownMbps: down * 8 / 1e6, UpPPS: upP, DownPPS: downP}
	l.TotalMbps, l.TotalPPS = l.UpMbps+l.DownMbps, l.UpPPS+l.DownPPS
	return l
}

// offeredMbps is the upstream media payload bitrate of all participants (JPEG and PCM
// bytes only, no headers or parity), kept for comparison with earlier results.
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
	MaxFrameBytes        int                `json:"max_frame_bytes"`
	OfferedUpMbps        float64            `json:"offered_upstream_mbps"` // media payload only
	Traffic              wireReport         `json:"wire_traffic"`
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
	ServerCore           *coreReport        `json:"server_core,omitempty"`
	LoadgenCPUSeconds    float64            `json:"loadgen_cpu_seconds"`
	LoadgenCPUUtil       float64            `json:"loadgen_cpu_utilisation"`
	LoadgenGOMAXPROCS    int                `json:"loadgen_gomaxprocs"`
	ControlLost          int                `json:"control_connections_lost"`
	StartedUTC           string             `json:"started_utc"`
}

// coreReport is what /proc/stat says the server's CPU did during the window, whoever it
// was charged to.
type coreReport struct {
	CPU            int     `json:"cpu"`
	BusySeconds    float64 `json:"busy_seconds"`    // all but idle and iowait
	SoftirqSeconds float64 `json:"softirq_seconds"` // softirq time accounted as such (see kernel_config)
	Utilisation    float64 `json:"utilisation"`
}

// wireReport is the run's traffic on the wire: the estimate for the average and for
// the largest corpus frame (the bound that was checked against the limits), the limits,
// and what the load generator measured it sent.
type wireReport struct {
	Expected   load    `json:"estimate_average_frame"`
	Bound      load    `json:"estimate_largest_frame"`
	LimitMbps  float64 `json:"limit_total_mbps"`
	LimitPPS   float64 `json:"limit_total_pps"`
	SentUpMbps float64 `json:"measured_upstream_mbps"`
	SentUpPPS  float64 `json:"measured_upstream_pps"`
}

func main() {
	err := run(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return // -h: the usage text has been printed
	}
	if err != nil {
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
	largest := 0
	for _, f := range frames {
		total += float64(len(f))
		largest = max(largest, len(f))
	}
	avg := total / float64(len(frames))
	if err := cfg.validate(largest); err != nil {
		return err
	}
	bound := cfg.estimate(largest)
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

	// The watchdog stops every sender if what they send is more than the estimate
	// allows; the limits were checked against that estimate.
	var tripped atomic.Bool
	go watchdog(ctx, stats, bound, &tripped)

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
			sendLoop(clk, cfg, frames, i, p, st, period, audioPeriod, winStart, winEnd, stopSend, &tripped)
		}(i, p, stats[i])
	}

	var cpu0, cpu1 float64
	var haveCPU bool
	var core0, core1 coreTimes
	var core0Err, core1Err error = errors.New("not read"), errors.New("not read")
	sleepUntil(clk, winStart)
	if cfg.ServerPID > 0 {
		if v, err := procCPUSeconds(cfg.ServerPID); err == nil {
			cpu0, haveCPU = v, true
		}
	}
	if cfg.ServerCore >= 0 {
		core0, core0Err = readCoreTimes(cfg.ServerCore)
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
	if cfg.ServerCore >= 0 {
		core1, core1Err = readCoreTimes(cfg.ServerCore)
	}
	selfWin1 := selfCPUSeconds()
	lost := 0
	for _, p := range parts {
		if p.controlLost() {
			lost++
		}
	}
	swg.Wait()
	sendEnd := clk.now()
	recvCancel()
	rwg.Wait()
	if tripped.Load() {
		return errors.New("stopped: the load generator sent more than its traffic estimate allows")
	}
	maxMbps, maxPPS := cfg.limits()

	res := result{
		Label:         cfg.Label,
		Wire:          cfg.Wire,
		Participants:  cfg.Participants,
		Rooms:         (cfg.Participants + cfg.RoomSize - 1) / cfg.RoomSize,
		RoomSize:      cfg.RoomSize,
		FPS:           cfg.FPS,
		Audio:         cfg.Audio,
		WindowSeconds: cfg.Duration.Seconds(),
		AvgFrameBytes: math.Round(avg),
		MaxFrameBytes: largest,
		OfferedUpMbps: cfg.offeredMbps(avg),
		Traffic: wireReport{
			Expected:  cfg.estimate(int(math.Round(avg))),
			Bound:     bound,
			LimitMbps: maxMbps,
			LimitPPS:  maxPPS,
		},
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
	if secs := float64(sendEnd-t0) / 1e9; secs > 0 {
		res.Traffic.SentUpMbps = float64(res.BytesSent+ipUDPHeader*res.PacketsSent) * 8 / 1e6 / secs
		res.Traffic.SentUpPPS = float64(res.PacketsSent) / secs
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
	if core0Err == nil && core1Err == nil {
		busy := core1.busy - core0.busy
		res.ServerCore = &coreReport{
			CPU:            cfg.ServerCore,
			BusySeconds:    busy,
			SoftirqSeconds: core1.softirq - core0.softirq,
			Utilisation:    busy / cfg.Duration.Seconds(),
		}
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

// watchdog checks once a second what all senders put on the wire (with IP and UDP
// headers) against the upstream estimate for the largest frame, and trips when either
// rate is more than watchdogSlack times that.
func watchdog(ctx context.Context, stats []*senderStats, bound load, tripped *atomic.Bool) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastBytes, lastPkts int64
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var b, p int64
			for _, st := range stats {
				b += st.bytes.Load()
				p += st.packets.Load()
			}
			secs := now.Sub(last).Seconds()
			mbps := float64(b-lastBytes+ipUDPHeader*(p-lastPkts)) * 8 / 1e6 / secs
			pps := float64(p-lastPkts) / secs
			if tooFast(mbps, pps, bound) {
				tripped.Store(true)
				return
			}
			lastBytes, lastPkts, last = b, p, now
		}
	}
}

// tooFast reports whether a measured upstream rate is over the watchdog's allowance
// (a little slack above the estimate, for timer jitter).
func tooFast(mbps, pps float64, bound load) bool {
	return mbps > watchdogSlack*bound.UpMbps+1 || pps > watchdogSlack*bound.UpPPS+100
}

func sendLoop(clk clock, cfg config, frames [][]byte, i int, p participant, st *senderStats,
	period, audioPeriod time.Duration, winStart, winEnd, stop int64, tripped *atomic.Bool) {
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
		if next >= stop || tripped.Load() {
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

// coreTimes is one CPU's line of /proc/stat, in seconds.
type coreTimes struct{ busy, softirq float64 }

func readCoreTimes(cpu int) (coreTimes, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return coreTimes{}, err
	}
	return parseCoreTimes(string(b), cpu)
}

// parseCoreTimes reads "cpuN user nice system idle iowait irq softirq steal ..." (in
// USER_HZ ticks).
func parseCoreTimes(stat string, cpu int) (coreTimes, error) {
	prefix := fmt.Sprintf("cpu%d ", cpu)
	for _, line := range strings.Split(stat, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 8 {
			break
		}
		var v [8]uint64
		for i := range v {
			x, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return coreTimes{}, fmt.Errorf("malformed /proc/stat line %q", line)
			}
			v[i] = x
		}
		busy := v[0] + v[1] + v[2] + v[5] + v[6] + v[7] // user nice system irq softirq steal
		return coreTimes{busy: float64(busy) / clkTck, softirq: float64(v[6]) / clkTck}, nil
	}
	return coreTimes{}, fmt.Errorf("no cpu%d line in /proc/stat", cpu)
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
