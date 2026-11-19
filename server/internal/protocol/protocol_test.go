package protocol

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestRTPRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		h    RTPHeader
	}{
		{"zero", RTPHeader{}},
		{"video marker", RTPHeader{Marker: true, PayloadType: PTVideo, SequenceNumber: 65535, Timestamp: 1<<32 - 1, SSRC: 1}},
		{"audio", RTPHeader{PayloadType: PTAudio, SequenceNumber: 1, Timestamp: 320, SSRC: 0xdeadbeef}},
		{"max csrc", RTPHeader{PayloadType: PTAudio, CSRCCount: MaxCSRC, CSRC: [MaxCSRC]uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := make([]byte, 128)
			n, err := tt.h.MarshalTo(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.h.HeaderSize() {
				t.Fatalf("n = %d, HeaderSize = %d", n, tt.h.HeaderSize())
			}
			pkt := append(buf[:n:n], 0xAB)
			var got RTPHeader
			payload, err := ParseRTP(pkt, &got)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.h {
				t.Fatalf("got %+v want %+v", got, tt.h)
			}
			if !bytes.Equal(payload, []byte{0xAB}) {
				t.Fatalf("payload %x", payload)
			}
		})
	}
}

func TestRTPMarshalRejects(t *testing.T) {
	buf := make([]byte, 64)
	for _, h := range []RTPHeader{
		{CSRCCount: 16},
		{PayloadType: 128},
		{Padding: true},
		{Extension: true},
	} {
		if _, err := h.MarshalTo(buf); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", h, err)
		}
	}
	h := RTPHeader{CSRCCount: 2}
	if _, err := h.MarshalTo(buf[:15]); !errors.Is(err, ErrBufferSize) {
		t.Errorf("short buffer: err = %v", err)
	}
}

func TestSeqDiff(t *testing.T) {
	tests := []struct {
		a, b uint16
		want int
	}{
		{5, 3, 2},
		{3, 5, -2},
		{0, 65535, 1},
		{65535, 0, -1},
		{100, 100, 0},
		{32767, 0, 32767},
	}
	for _, tt := range tests {
		if got := SeqDiff(tt.a, tt.b); got != tt.want {
			t.Errorf("SeqDiff(%d,%d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestVideoHeaderValidation(t *testing.T) {
	buf := make([]byte, 16)
	for _, v := range []VideoHeader{{FragmentCount: 0}, {FragmentIndex: 2, FragmentCount: 2}, {FragmentCount: MaxFragments + 1}} {
		if _, err := v.MarshalTo(buf); err == nil {
			t.Errorf("%+v accepted", v)
		}
	}
	big := make([]byte, VideoHeaderSize+MaxFragmentPayload+1)
	v := VideoHeader{FragmentCount: 1}
	if _, err := v.MarshalTo(big); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseVideo(big, &v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized fragment: err = %v", err)
	}
}

func TestFragmentCount(t *testing.T) {
	for _, tt := range []struct{ n, want int }{{1, 1}, {1200, 1}, {1201, 2}, {31111, 26}} {
		if got := FragmentCount(tt.n); got != tt.want {
			t.Errorf("FragmentCount(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

// makeStream builds n consecutive RTP packets with random payload lengths.
func makeStream(rng *rand.Rand, n int, ssrc uint32, seq0 uint16, maxBody int) [][]byte {
	pkts := make([][]byte, n)
	for i := range pkts {
		h := RTPHeader{
			PayloadType:    PTVideo,
			SequenceNumber: seq0 + uint16(i),
			Timestamp:      rng.Uint32(),
			SSRC:           ssrc,
			Marker:         rng.IntN(2) == 0,
			CSRCCount:      uint8(rng.IntN(3)),
		}
		for j := 0; j < int(h.CSRCCount); j++ {
			h.CSRC[j] = rng.Uint32()
		}
		body := make([]byte, 1+rng.IntN(maxBody-4*int(h.CSRCCount)))
		for j := range body {
			body[j] = byte(rng.Uint32())
		}
		buf := make([]byte, h.HeaderSize()+len(body))
		n, _ := h.MarshalTo(buf)
		copy(buf[n:], body)
		pkts[i] = buf
	}
	return pkts
}

func TestFECRecoversAnySingleLoss(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, k := range []int{1, 2, 3, 4, 8, MaxFECGroup} {
		for trial := 0; trial < 20; trial++ {
			seq0 := uint16(rng.Uint32())
			pkts := makeStream(rng, k, 77, seq0, MaxProtectedBody)
			var enc FECEncoder
			for _, p := range pkts {
				if err := enc.Add(p); err != nil {
					t.Fatalf("k=%d Add: %v", k, err)
				}
			}
			parity := make([]byte, MaxSendDatagram)
			n, err := enc.Flush(parity, 77, seq0+uint16(k))
			if err != nil {
				t.Fatal(err)
			}
			if n > MaxSendDatagram {
				t.Fatalf("parity packet of %d bytes exceeds %d", n, MaxSendDatagram)
			}
			for missing := 0; missing < k; missing++ {
				var rec FECRecovery
				if err := rec.Start(parity[:n]); err != nil {
					t.Fatal(err)
				}
				for i, p := range pkts {
					if i != missing {
						if err := rec.Add(p); err != nil {
							t.Fatal(err)
						}
					}
				}
				out := make([]byte, MaxSendDatagram)
				m, err := rec.Finish(out, seq0+uint16(missing))
				if err != nil {
					t.Fatalf("k=%d missing=%d: %v", k, missing, err)
				}
				if !bytes.Equal(out[:m], pkts[missing]) {
					t.Fatalf("k=%d missing=%d: wrong packet", k, missing)
				}
			}
		}
	}
}

func TestFECEncoderResetsBetweenGroups(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	seq0 := uint16(65533) // the group wraps around 65535
	pkts := makeStream(rng, 6, 9, seq0, 300)
	var enc FECEncoder
	out := make([]byte, MaxSendDatagram)
	var parities [][]byte
	for i, p := range pkts {
		if err := enc.Add(p); err != nil {
			t.Fatal(err)
		}
		if enc.Len() == 3 {
			n, err := enc.Flush(out, 9, uint16(i))
			if err != nil {
				t.Fatal(err)
			}
			parities = append(parities, append([]byte(nil), out[:n]...))
		}
	}
	// The second group must recover on its own, proving that state was reset.
	var rec FECRecovery
	if err := rec.Start(parities[1]); err != nil {
		t.Fatal(err)
	}
	_ = rec.Add(pkts[3])
	_ = rec.Add(pkts[5])
	m, err := rec.Finish(out, seq0+4)
	if err != nil || !bytes.Equal(out[:m], pkts[4]) {
		t.Fatalf("second group recovery failed: %v", err)
	}
}

func TestFECRejectsMisuse(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	pkts := makeStream(rng, 3, 1, 10, 200)
	var enc FECEncoder
	if _, err := enc.Flush(make([]byte, 100), 1, 0); err == nil {
		t.Fatal("flush of an empty group succeeded")
	}
	_ = enc.Add(pkts[0])
	if err := enc.Add(pkts[2]); err == nil {
		t.Fatal("non-consecutive packet accepted")
	}
	out := make([]byte, MaxSendDatagram)
	n, _ := enc.Flush(out, 1, 11)
	var rec FECRecovery
	if err := rec.Start(out[:n]); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Finish(out, 10); err != nil {
		t.Fatalf("k=1 group should rebuild its only packet: %v", err)
	}
	if err := rec.Start(pkts[0]); err == nil {
		t.Fatal("Start accepted a non-FEC packet")
	}
}

func TestReadFrameHandlesPartialReads(t *testing.T) {
	var stream bytes.Buffer
	msgs := []any{
		Error{Type: TypeError, Code: CodeBadRequest, Message: "x"},
		Pong{Type: TypePong, ServerTimeMS: 5},
	}
	for _, m := range msgs {
		f, err := EncodeFrame(m)
		if err != nil {
			t.Fatal(err)
		}
		stream.Write(f)
	}
	// iotest-style reader returning one byte at a time.
	r := &oneByteReader{r: &stream}
	var buf []byte
	for i := range msgs {
		body, err := ReadFrame(r, buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		req, err := DecodeRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if req.Type == "" {
			t.Fatal("empty type")
		}
	}
	if _, err := ReadFrame(r, buf); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestReadFrameErrors(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 5, '{'}), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body: %v", err)
	}
	if _, err := EncodeFrame(Error{Message: strings.Repeat("x", MaxControlMessage)}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized encode: %v", err)
	}
}

func TestTokenParse(t *testing.T) {
	var tok Token
	for i := range tok {
		tok[i] = byte(i * 7)
	}
	got, err := ParseToken(tok.String())
	if err != nil || got != tok {
		t.Fatalf("round trip failed: %v", err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("zz", 16), strings.Repeat("a", 33)} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) succeeded", bad)
		}
	}
}
