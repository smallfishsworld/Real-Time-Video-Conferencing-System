package protocol

import (
	"bytes"
	"slices"
	"testing"
)

// The fuzz targets check that no decoder panics or reads out of bounds, and that
// whatever a decoder accepts survives an encode/decode round trip.

func FuzzParseRTP(f *testing.F) {
	f.Add([]byte{0x80, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0xAA})
	f.Add([]byte{0xB2, 0xE1, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 1, 2})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		var h RTPHeader
		payload, err := ParseRTP(pkt, &h)
		if err != nil {
			return
		}
		if len(payload) > len(pkt) {
			t.Fatal("payload longer than packet")
		}
		if h.Padding || h.Extension {
			return
		}
		buf := make([]byte, h.HeaderSize()+len(payload))
		n, err := h.MarshalTo(buf)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		copy(buf[n:], payload)
		if !bytes.Equal(buf, pkt) {
			t.Fatalf("round trip mismatch")
		}
	})
}

func FuzzParseVideo(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 1, 0xFF})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x03, 0xFF, 0x04, 0x00, 1, 2, 3})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var v VideoHeader
		frag, err := ParseVideo(payload, &v)
		if err != nil {
			return
		}
		if v.FragmentIndex >= v.FragmentCount || len(frag) == 0 || len(frag) > MaxFragmentPayload {
			t.Fatalf("accepted invalid header %+v (fragment %d bytes)", v, len(frag))
		}
		buf := make([]byte, VideoHeaderSize)
		if _, err := v.MarshalTo(buf); err != nil || !bytes.Equal(buf, payload[:VideoHeaderSize]) {
			t.Fatalf("round trip mismatch: %v", err)
		}
	})
}

func FuzzParseFEC(f *testing.F) {
	f.Add([]byte{0, 100, 3, 0, 0, 0xE0, 0, 0x3F, 0, 0, 0x23, 0x28, 1, 2, 3})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var h FECHeader
		parity, err := ParseFEC(payload, &h)
		if err != nil {
			return
		}
		if h.Count == 0 || h.Count > MaxFECGroup || len(parity) > MaxProtectedBody {
			t.Fatalf("accepted invalid header %+v", h)
		}
		buf := make([]byte, FECHeaderSize)
		if _, err := h.MarshalTo(buf); err != nil {
			t.Fatal(err)
		}
		var h2 FECHeader
		if _, err := ParseFEC(buf, &h2); err != nil || h2 != h {
			t.Fatalf("round trip mismatch: %+v vs %+v (%v)", h2, h, err)
		}
	})
}

func FuzzFECRecovery(f *testing.F) {
	f.Add([]byte{0x80, 0x7f, 0, 3, 0, 0, 0, 1, 0, 0, 0, 1, 0, 1, 2, 0, 0, 0x60, 0, 4, 0, 0, 0, 0, 1, 2, 3, 4},
		[]byte{0x80, 0x60, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 9, 9})
	f.Fuzz(func(t *testing.T, parity, pkt []byte) {
		var rec FECRecovery
		if err := rec.Start(parity); err != nil {
			return
		}
		_ = rec.Add(pkt)
		out := make([]byte, MaxSendDatagram)
		hdr := rec.Header()
		n, err := rec.Finish(out, hdr.SNBase)
		if err == nil && (n < RTPHeaderSize || n > MaxSendDatagram) {
			t.Fatalf("recovered packet of impossible size %d", n)
		}
	})
}

func FuzzParseRegister(f *testing.F) {
	f.Add(append([]byte{1, 1, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}, make([]byte, 16)...))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r Register
		if err := ParseRegister(b, &r); err != nil {
			return
		}
		buf := make([]byte, RegisterSize)
		if _, err := r.MarshalTo(buf); err != nil {
			t.Fatal(err)
		}
		var r2 Register
		if err := ParseRegister(buf, &r2); err != nil || r2 != r {
			t.Fatalf("round trip mismatch")
		}
	})
}

func FuzzParseRegisterAck(f *testing.F) {
	f.Add([]byte{2, 1, 4, 0, 0, 0, 0, 0, 0, 0, 0, 42, 0xC3, 0x50, 127, 0, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		var a RegisterAck
		if err := ParseRegisterAck(b, &a); err != nil {
			return
		}
		if !a.Observed.IsValid() {
			t.Fatal("accepted an invalid address")
		}
		buf := make([]byte, 32)
		if _, err := a.MarshalTo(buf); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
	})
}

func FuzzControlFrame(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x10{\"type\":\"hello\"}"))
	f.Add([]byte("\x00\x00\x00\x02{}"))
	f.Fuzz(func(t *testing.T, b []byte) {
		body, err := ReadFrame(bytes.NewReader(b), nil)
		if err != nil {
			return
		}
		if len(body) == 0 || len(body) > MaxControlMessage {
			t.Fatalf("bad body length %d", len(body))
		}
		if req, err := DecodeRequest(body); err == nil && req.Type == "" {
			t.Fatal("accepted a request without type")
		}
	})
}

func FuzzParseNACK(f *testing.F) {
	f.Add([]byte{0x81, 205, 0, 3, 0, 0, 0, 1, 0, 0, 0, 2, 0, 7, 0x80, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		var n NACK
		if err := ParseNACK(b, &n); err != nil {
			return
		}
		buf := make([]byte, len(b))
		size, err := n.MarshalTo(buf)
		if err != nil || size != len(b) {
			t.Fatalf("MarshalTo = %d, %v for a %d-byte NACK", size, err, len(b))
		}
		var m NACK
		if err := ParseNACK(buf, &m); err != nil || m != n {
			t.Fatalf("round trip mismatch")
		}
		// A NACK from the wire may list sequence numbers in any order and more than
		// once; SetLost takes them in increasing order, as a receiver produces them.
		lost := n.AppendLost(nil)
		if !increasing(lost) {
			return
		}
		var p NACK
		if p.SetLost(lost) != len(lost) || !slices.Equal(p.AppendLost(nil), lost) {
			t.Fatalf("SetLost/AppendLost disagree on %v", lost)
		}
	})
}

// increasing reports whether every sequence number in s follows the one before it
// (modulo 2^16, within half the sequence space).
func increasing(s []uint16) bool {
	for i := 1; i < len(s); i++ {
		if d := s[i] - s[i-1]; d == 0 || d >= 1<<15 {
			return false
		}
	}
	return true
}
