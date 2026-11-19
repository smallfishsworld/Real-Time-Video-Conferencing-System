package protocol

import (
	"testing"
)

// videoFragment returns a full-size video fragment (RTP + video header + payload).
func videoFragment(b *testing.B, seq uint16) []byte {
	b.Helper()
	pkt := make([]byte, RTPHeaderSize+VideoHeaderSize+MaxFragmentPayload)
	h := RTPHeader{PayloadType: PTVideo, SequenceNumber: seq, Timestamp: 90000, SSRC: 0xCAFE}
	n, err := h.MarshalTo(pkt)
	if err != nil {
		b.Fatal(err)
	}
	v := VideoHeader{FrameID: 7, FragmentIndex: seq % 26, FragmentCount: 26}
	m, err := v.MarshalTo(pkt[n:])
	if err != nil {
		b.Fatal(err)
	}
	for i := n + m; i < len(pkt); i++ {
		pkt[i] = byte(i * 13)
	}
	return pkt
}

func BenchmarkParseRTPVideoFragment(b *testing.B) {
	pkt := videoFragment(b, 1)
	b.ReportAllocs()
	var h RTPHeader
	var v VideoHeader
	for b.Loop() {
		payload, err := ParseRTP(pkt, &h)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ParseVideo(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalRTPHeader(b *testing.B) {
	buf := make([]byte, MaxSendDatagram)
	h := RTPHeader{PayloadType: PTAudio, SequenceNumber: 1, Timestamp: 320, SSRC: 0xBEEF, CSRCCount: 3}
	b.ReportAllocs()
	for b.Loop() {
		h.SequenceNumber++
		if _, err := h.MarshalTo(buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFECEncodeVideoGroup builds the parity packet of one k=4 group of full-size
// video fragments, as a sender does for every four fragments.
func BenchmarkFECEncodeVideoGroup(b *testing.B) {
	const k = 4
	pkts := make([][]byte, k)
	for i := range pkts {
		pkts[i] = videoFragment(b, uint16(100+i))
	}
	out := make([]byte, MaxSendDatagram+FECHeaderSize)
	var enc FECEncoder
	b.SetBytes(int64(k * len(pkts[0])))
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range pkts {
			if err := enc.Add(p); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := enc.Flush(out, 0xCAFE, 104); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFECRecoverVideoFragment rebuilds one lost fragment of a k=4 group.
func BenchmarkFECRecoverVideoFragment(b *testing.B) {
	const k = 4
	pkts := make([][]byte, k)
	var enc FECEncoder
	for i := range pkts {
		pkts[i] = videoFragment(b, uint16(100+i))
		if err := enc.Add(pkts[i]); err != nil {
			b.Fatal(err)
		}
	}
	parity := make([]byte, MaxSendDatagram+FECHeaderSize)
	n, err := enc.Flush(parity, 0xCAFE, 104)
	if err != nil {
		b.Fatal(err)
	}
	parity = parity[:n]
	out := make([]byte, MaxSendDatagram)
	var rec FECRecovery
	b.SetBytes(int64(len(pkts[0])))
	b.ReportAllocs()
	for b.Loop() {
		if err := rec.Start(parity); err != nil {
			b.Fatal(err)
		}
		for _, p := range pkts[1:] { // packet 0 is missing
			if err := rec.Add(p); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := rec.Finish(out, 100); err != nil {
			b.Fatal(err)
		}
	}
}
