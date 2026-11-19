package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
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
	cases := []struct {
		name   string
		mutate func(*config)
		errSub string
	}{
		{"defaults are fine", func(*config) {}, ""},
		{"remote target refused", func(c *config) { c.Server = "192.0.2.10" }, "non-loopback"},
		{"remote target allowed by flag", func(c *config) { c.Server = "192.0.2.10"; c.AllowRemote = true }, ""},
		{"host names refused", func(c *config) { c.Server = "example.com" }, "IP address"},
		{"too many participants", func(c *config) { c.Participants = hardMaxParticipants + 1 }, "participants"},
		{"legacy port scheme", func(c *config) { c.Wire = "legacy"; c.Participants = legacyMaxID + 1 }, "legacy"},
		{"bitrate cap", func(c *config) { c.Participants = 500; c.MaxMbps = 100 }, "exceeds"},
		{"hard bitrate cap", func(c *config) { c.MaxMbps = hardMaxMbps + 1 }, "hard cap"},
		{"duration cap", func(c *config) { c.Duration = time.Hour }, "longer"},
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

func TestRoomMembers(t *testing.T) {
	cfg := config{Participants: 10, RoomSize: 4}
	want := []int{4, 4, 4, 4, 4, 4, 4, 4, 2, 2}
	for i, w := range want {
		if got := roomMembers(cfg, i); got != w {
			t.Errorf("roomMembers(%d) = %d, want %d", i, got, w)
		}
	}
}
