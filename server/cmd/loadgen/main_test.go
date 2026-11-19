package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/client"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Hashes of pickle.dumps((id, kind, data)) from CPython 3.13 (default protocol 4),
// where data[j] = (j*31 + n) & 0xff.
func TestPickleMatchesCPython(t *testing.T) {
	cases := []struct {
		id, kind, n int
		sha         string
		size        int
	}{
		{id: 5, kind: 1, n: 100, sha: "417bdb19d8cfedd6d68a74420b3c6e8b63dabf8bfaf8f69dce6d29cde3c62eee", size: 121},
		{id: 5, kind: 2, n: 2048, sha: "8f272d844988a19268499e6a4d6d7805ac13558cc6ba8e9385bfee5430ba74ac", size: 2072},
		{id: 300, kind: 1, n: 31000, sha: "5783126aba628a77eabc3e9a437aee42926afb4a68497a6b89d14f16eae9a2e7", size: 31025},
		{id: 452, kind: 2, n: 300, sha: "34058c7135cd8115be2f36d0a1628e8fc2efa1c374b06823f15c340e3966d542", size: 325},
		{id: 7, kind: 1, n: 255, sha: "87ea5043b5d084de29f0c6a6757a9b43575617ad38ba76604993f03eb8b919e3", size: 276},
		{id: 7, kind: 1, n: 256, sha: "a521c58f8425b1a341ce8085964216c0381c486028c639716563e1737f7a5b32", size: 280},
	}
	for _, c := range cases {
		data := make([]byte, c.n)
		for j := range data {
			data[j] = byte(j*31 + c.n)
		}
		got := appendPickle(nil, c.id, c.kind, data)
		sum := sha256.Sum256(got)
		if len(got) != c.size || hex.EncodeToString(sum[:]) != c.sha {
			t.Errorf("id=%d kind=%d n=%d: size %d (want %d), sha %x", c.id, c.kind, c.n, len(got), c.size, sum)
		}
		id, kind, payload, ok := parsePickle(got)
		if !ok || id != c.id || kind != c.kind || string(payload) != string(data) {
			t.Errorf("parsePickle round trip failed for id=%d kind=%d n=%d", c.id, c.kind, c.n)
		}
	}
}

func TestParsePickleRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("hello world, not a pickle"), make([]byte, 64)} {
		if _, _, _, ok := parsePickle(b); ok {
			t.Errorf("parsePickle accepted %q", b)
		}
	}
	good := appendPickle(nil, 1, 1, make([]byte, 500))
	if _, _, _, ok := parsePickle(good[:len(good)-200]); ok {
		t.Error("parsePickle accepted a truncated datagram")
	}
}

func TestTagRoundTrip(t *testing.T) {
	frame := append([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0x00, 0x22}, make([]byte, 64)...)
	want := tag{Sender: 77, Seq: 123456, SentNS: 987654321012}
	writeTag(frame, want)
	got, ok := readTag(frame)
	if !ok || got != want {
		t.Fatalf("readTag = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := readTag(frame[:20]); ok {
		t.Error("readTag accepted a short frame")
	}
	frame[3] = 0xE0 // APP0 instead of COM
	if _, ok := readTag(frame); ok {
		t.Error("readTag accepted a frame without COM")
	}
}

func TestParseStatCPU(t *testing.T) {
	stat := "1234 (conf server) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 8 0 100 0 0"
	got, err := parseStatCPU(stat)
	if err != nil || got != 3.0 {
		t.Fatalf("parseStatCPU = %v, %v; want 3.0", got, err)
	}
	if _, err := parseStatCPU("garbage"); err == nil {
		t.Error("parseStatCPU accepted garbage")
	}
}

func TestValidateSafetyLimits(t *testing.T) {
	base := func() config {
		c, err := parseFlags(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	remote := func(c *config) { c.Server = "192.0.2.10"; c.AllowRemote = true }
	cases := []struct {
		name   string
		mutate func(*config)
		errSub string
	}{
		{"defaults are fine", func(*config) {}, ""},
		{"remote target refused", func(c *config) { c.Server = "192.0.2.10" }, "non-loopback"},
		{"remote target allowed by flag", func(c *config) { remote(c); c.Participants = 2 }, ""},
		{"remote default bitrate is low", func(c *config) { remote(c); c.Participants = 8 }, "exceeds -max-mbps 25"},
		{"remote limits raised by flags", func(c *config) {
			remote(c)
			c.Participants, c.FPS, c.MaxMbps, c.MaxPPS = 4, 5, 100, 5000
		}, ""},
		{"remote default packet rate", func(c *config) {
			remote(c)
			c.Participants, c.RoomSize, c.FPS = 16, 16, 0.1 // mostly audio: few bits, many packets
		}, "exceeds -max-pps 2500"},
		{"remote hard bitrate cap", func(c *config) { remote(c); c.MaxMbps = hardRemoteMbps + 1 }, "hard cap of 100"},
		{"remote hard packet cap", func(c *config) { remote(c); c.MaxPPS = hardRemotePPS + 1 }, "hard cap of 5000"},
		{"remote room size", func(c *config) { remote(c); c.RoomSize = 17 }, "room-size above 16"},
		{"remote participants", func(c *config) { remote(c); c.Participants = 65 }, "more than 64"},
		{"host names refused", func(c *config) { c.Server = "example.com" }, "IP address"},
		{"too many participants", func(c *config) { c.Participants = hardMaxParticipants + 1 }, "participants"},
		{"legacy port scheme", func(c *config) { c.Wire = "legacy"; c.Participants = legacyMaxID + 1 }, "legacy"},
		{"bitrate cap counts fan-out and headers", func(c *config) { c.Participants = 64; c.MaxMbps = 600 }, "exceeds -max-mbps 600"},
		{"packet-rate cap", func(c *config) { c.Participants = 64; c.MaxPPS = 50_000 }, "exceeds -max-pps"},
		{"hard bitrate cap", func(c *config) { c.MaxMbps = hardLoopbackMbps + 1 }, "hard cap"},
		{"NaN bitrate", func(c *config) { c.MaxMbps = math.NaN() }, "-max-mbps"},
		{"infinite bitrate", func(c *config) { c.MaxMbps = math.Inf(1) }, "-max-mbps"},
		{"negative bitrate", func(c *config) { c.MaxMbps = -1 }, "-max-mbps"},
		{"NaN packet rate", func(c *config) { c.MaxPPS = math.NaN() }, "-max-pps"},
		{"NaN fps", func(c *config) { c.FPS = math.NaN() }, "-fps"},
		{"tiny fps", func(c *config) { c.FPS = 1e-9 }, "-fps"},
		{"infinite fps", func(c *config) { c.FPS = math.Inf(1) }, "-fps"},
		{"FEC group too large", func(c *config) { c.FECK = protocol.MaxFECGroup + 1 }, "-fec-k"},
		{"negative audio FEC group", func(c *config) { c.AudioFECK = -1 }, "-fec-k"},
		{"negative warmup", func(c *config) { c.Warmup = -time.Second }, "-warmup"},
		{"negative grace", func(c *config) { c.Grace = -time.Second }, "-grace"},
		{"zero duration", func(c *config) { c.Duration = 0 }, "-duration"},
		{"overflowing sum", func(c *config) { c.Warmup = math.MaxInt64 / 2; c.Duration = math.MaxInt64 / 2 }, "-warmup"},
		{"duration cap", func(c *config) { c.Duration = time.Hour }, "-duration"},
		{"total duration cap", func(c *config) { c.Warmup = 6 * time.Minute; c.Duration = 6 * time.Minute }, "longer"},
		{"room size", func(c *config) { c.RoomSize = 1 }, "-room-size"},
		{"unknown wire", func(c *config) { c.Wire = "rtmp" }, "wire"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.validate(31000)
			switch {
			case tc.errSub == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.errSub != "" && (err == nil || !strings.Contains(err.Error(), tc.errSub)):
				t.Fatalf("error %v, want it to mention %q", err, tc.errSub)
			}
		})
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	if _, err := parseFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) = %v, want flag.ErrHelp (main exits 0 on it)", err)
	}
}

// sink is a UDP socket that a v2Participant sends to; it returns the datagram sizes.
func sink(t *testing.T) (*net.UDPConn, func(n int) []int) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadBuffer(4 << 20)
	read := func(n int) []int {
		out := make([]int, 0, n)
		buf := make([]byte, 65536)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for len(out) < n {
			k, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				t.Fatalf("read %d of %d datagrams: %v", len(out), n, err)
			}
			out = append(out, k)
		}
		return out
	}
	return conn, read
}

func testParticipant(t *testing.T, cfg config, to *net.UDPConn) *v2Participant {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &v2Participant{
		cfg:    cfg,
		pc:     ipv4.NewPacketConn(conn),
		joined: client.Message{VideoSSRC: 1, AudioSSRC: 2},
		server: to.LocalAddr().(*net.UDPAddr),
		st:     &senderStats{},
		msgs:   make([]ipv4.Message, batch),
	}
}

// The traffic estimate must describe what the load generator really sends.
func TestEstimateMatchesWhatIsSent(t *testing.T) {
	for _, k := range []int{0, 1, 2, 4, 7} {
		for _, n := range []int{1, 1199, 1200, 1201, 4800, 4801, 31000, 57601} {
			cfg := config{Wire: "v2", Server: "127.0.0.1", FECK: k}
			to, read := sink(t)
			p := testParticipant(t, cfg, to)
			frame := make([]byte, n)
			if err := p.sendVideo(frame, tag{Seq: 1}); err != nil {
				t.Fatal(err)
			}
			want := cfg.v2Frame(n)
			got := read(len(want))
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("k=%d n=%d: sent %v, estimate %v", k, n, got, want)
			}
		}
	}
	cfg := config{Wire: "v2", Server: "127.0.0.1", AudioFECK: 2}
	to, read := sink(t)
	p := testParticipant(t, cfg, to)
	for seq := uint32(0); seq < 4; seq++ {
		if err := p.sendAudio(seq); err != nil {
			t.Fatal(err)
		}
	}
	got := read(6)
	media, parity := protocol.RTPHeaderSize+v2AudioPayloadBytes, protocol.RTPHeaderSize+protocol.FECHeaderSize+v2AudioPayloadBytes
	if want := fmt.Sprint([]int{media, media, parity, media, media, parity}); fmt.Sprint(got) != want {
		t.Errorf("audio sent %v, want %v", got, want)
	}
}

func TestEstimateCountsFanOutHeadersAndFragments(t *testing.T) {
	c := config{Wire: "legacy", Server: "127.0.0.1", Participants: 4, RoomSize: 4, FPS: 10}
	l := c.estimate(10000)
	// Up: 4 senders x 10 datagrams/s of 10000 + pickle + 28 bytes; down: 3 copies each.
	up := 4 * 10 * float64(pickleSize(10000)+ipUDPHeader) * 8 / 1e6
	if math.Abs(l.UpMbps-up) > 1e-9 || math.Abs(l.DownMbps-3*up) > 1e-9 || l.UpPPS != 40 || l.DownPPS != 120 {
		t.Fatalf("legacy estimate %+v, want up %.3f Mbit/s and 40 + 120 packets/s", l, up)
	}
	c.Server, c.AllowRemote = "192.0.2.1", true // on a real network: IP fragments
	frags := float64((pickleSize(10000) + 8 + remoteMTU - ipHeader - 1) / (remoteMTU - ipHeader))
	if r := c.estimate(10000); r.UpPPS != 40*frags || r.UpMbps <= l.UpMbps {
		t.Fatalf("remote legacy estimate %+v, want %v fragments per datagram", r, frags)
	}
	v := config{Wire: "v2", Server: "127.0.0.1", Participants: 3, RoomSize: 4, FPS: 15, FECK: 4, Audio: true, AudioFECK: 2}
	e := v.estimate(31000)
	// 26 fragments and 7 parity packets per frame; 50 audio packets/s and 25 parity.
	videoUp, audio := 3*15*33.0, 3*75.0
	if e.UpPPS != videoUp+audio || e.DownPPS != 2*videoUp+audio || e.TotalPPS != e.UpPPS+e.DownPPS {
		t.Fatalf("v2 estimate %+v: want %v packets/s up, and down two copies of the video plus "+
			"one mixed audio stream per member", e, videoUp+audio)
	}
}

func TestWatchdogAllowance(t *testing.T) {
	bound := load{UpMbps: 100, UpPPS: 10_000}
	if tooFast(100, 10_000, bound) || tooFast(120, 12_000, bound) {
		t.Error("the estimate itself must pass")
	}
	if !tooFast(130, 1000, bound) || !tooFast(1, 13_000, bound) {
		t.Error("25 % over the estimate must trip")
	}
}

func TestRoomMembers(t *testing.T) {
	cfg := config{Participants: 10, RoomSize: 4}
	want := []int{4, 4, 4, 4, 4, 4, 4, 4, 2, 2}
	for i, w := range want {
		if got := roomMembers(cfg, i); got != w {
			t.Errorf("roomMembers(%d) = %d, want %d", i, got, w)
		}
	}
}
