package protocol

import (
	"slices"
	"testing"
)

func TestNACKRoundTrip(t *testing.T) {
	lost := []uint16{65530, 65531, 65535, 3, 20, 40, 41, 56}
	var n NACK
	n.SenderSSRC, n.MediaSSRC = 0x11111111, 0x22222222
	if got := n.SetLost(lost); got != len(lost) {
		t.Fatalf("SetLost packed %d of %d", got, len(lost))
	}
	// 65530 covers up to 65530+16 = 10 (wrapping); 20 is next; 40 covers 41 and 56.
	want := []NACKItem{{65530, 1<<0 | 1<<4 | 1<<8}, {20, 0}, {40, 1<<0 | 1<<15}}
	if n.N != len(want) || !slices.Equal(n.Items[:n.N], want) {
		t.Fatalf("items %+v, want %+v", n.Items[:n.N], want)
	}
	buf := make([]byte, 128)
	size, err := n.MarshalTo(buf)
	if err != nil || size != NACKHeaderSize+4*len(want) {
		t.Fatalf("MarshalTo = %d, %v", size, err)
	}
	if !IsNACK(buf[:size]) || IsRTP(buf[:size]) && buf[1]&0x7f == PTVideo {
		t.Fatal("NACK not recognised")
	}
	var m NACK
	if err := ParseNACK(buf[:size], &m); err != nil {
		t.Fatal(err)
	}
	if m.SenderSSRC != n.SenderSSRC || m.MediaSSRC != n.MediaSSRC || !slices.Equal(m.Items[:m.N], want) {
		t.Fatalf("parsed %+v", m)
	}
	if got := m.AppendLost(nil); !slices.Equal(got, lost) {
		t.Fatalf("AppendLost = %v, want %v", got, lost)
	}
}

func TestNACKSetLostStopsWhenFull(t *testing.T) {
	var lost []uint16
	for i := range 40 {
		lost = append(lost, uint16(i*20)) // too far apart to share an item
	}
	var n NACK
	if got := n.SetLost(lost); got != MaxNACKItems || n.N != MaxNACKItems {
		t.Fatalf("SetLost = %d (N %d), want %d", got, n.N, MaxNACKItems)
	}
}

func TestParseNACKRejects(t *testing.T) {
	var n NACK
	n.SenderSSRC, n.MediaSSRC = 1, 2
	n.SetLost([]uint16{7})
	good := make([]byte, 16)
	if _, err := n.MarshalTo(good); err != nil {
		t.Fatal(err)
	}
	mod := func(f func(b []byte) []byte) []byte { return f(slices.Clone(good)) }
	for name, b := range map[string][]byte{
		"short":         good[:12],
		"padding":       mod(func(b []byte) []byte { b[0] |= 0x20; return b }),
		"wrong format":  mod(func(b []byte) []byte { b[0] = 0x80 | 15; return b }),
		"length beyond": mod(func(b []byte) []byte { b[3] = 4; return b }),
		"trailing data": append(slices.Clone(good), 0, 0, 0, 0),
		"no items":      mod(func(b []byte) []byte { b[3] = 2; return b[:12] }),
		"rtp":           {0x80, PTVideo | 0x80, 0, 1, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 0},
	} {
		var m NACK
		if err := ParseNACK(b, &m); err == nil {
			t.Errorf("%s: parsed %+v", name, m)
		}
	}
	many := make([]byte, NACKHeaderSize+4*(MaxNACKItems+1))
	copy(many, good[:12])
	many[3] = byte(len(many)/4 - 1)
	var m NACK
	if err := ParseNACK(many, &m); err == nil {
		t.Error("parsed a NACK with too many items")
	}
}
