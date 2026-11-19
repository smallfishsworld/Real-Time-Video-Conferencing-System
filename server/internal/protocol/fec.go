package protocol

import (
	"crypto/subtle"
	"encoding/binary"
)

// FEC limits (docs/PROTOCOL.md section 2.6).
const (
	FECHeaderSize = 12
	MaxFECGroup   = 48
	// MaxProtectedBody is the largest body (packet minus the 12-byte fixed header)
	// that a parity packet can protect while staying within MaxSendDatagram.
	MaxProtectedBody = MaxSendDatagram - RTPHeaderSize - FECHeaderSize
)

// FECHeader is the 12-byte header that follows the RTP header of a parity packet.
type FECHeader struct {
	SNBase    uint16
	Count     uint8
	Byte0Rec  uint8
	Byte1Rec  uint8
	LengthRec uint16
	TSRec     uint32
}

// Covers reports whether seq is one of the sequence numbers protected by f.
func (f *FECHeader) Covers(seq uint16) bool {
	d := seq - f.SNBase
	return d < uint16(f.Count)
}

// ParseFEC decodes the FEC header from an RTP payload and returns the parity bytes.
func ParseFEC(payload []byte, f *FECHeader) ([]byte, error) {
	if len(payload) < FECHeaderSize {
		return nil, ErrShort
	}
	f.SNBase = binary.BigEndian.Uint16(payload[0:2])
	f.Count = payload[2]
	f.Byte0Rec = payload[4]
	f.Byte1Rec = payload[5]
	f.LengthRec = binary.BigEndian.Uint16(payload[6:8])
	f.TSRec = binary.BigEndian.Uint32(payload[8:12])
	if f.Count == 0 || f.Count > MaxFECGroup || f.Byte0Rec&0xC0 != 0 {
		return nil, ErrInvalid
	}
	parity := payload[FECHeaderSize:]
	if len(parity) > MaxProtectedBody {
		return nil, ErrInvalid
	}
	return parity, nil
}

// MarshalTo writes the FEC header into buf.
func (f *FECHeader) MarshalTo(buf []byte) (int, error) {
	if len(buf) < FECHeaderSize {
		return 0, ErrBufferSize
	}
	if f.Count == 0 || f.Count > MaxFECGroup {
		return 0, ErrInvalid
	}
	binary.BigEndian.PutUint16(buf[0:2], f.SNBase)
	buf[2] = f.Count
	buf[3] = 0
	buf[4] = f.Byte0Rec
	buf[5] = f.Byte1Rec
	binary.BigEndian.PutUint16(buf[6:8], f.LengthRec)
	binary.BigEndian.PutUint32(buf[8:12], f.TSRec)
	return FECHeaderSize, nil
}

// FECEncoder accumulates XOR parity over consecutive packets of one RTP stream.
// The zero value is ready to use. It is not safe for concurrent use.
type FECEncoder struct {
	hdr     FECHeader
	lastTS  uint32
	maxBody int
	parity  [MaxProtectedBody]byte
}

// Len returns the number of packets in the current group.
func (e *FECEncoder) Len() int { return int(e.hdr.Count) }

// Add folds a complete RTP packet into the current group. Packets of a group must
// have consecutive sequence numbers.
func (e *FECEncoder) Add(pkt []byte) error {
	if len(pkt) < RTPHeaderSize || !IsRTP(pkt) {
		return ErrShort
	}
	body := pkt[RTPHeaderSize:]
	if len(body) > MaxProtectedBody {
		return ErrInvalid
	}
	seq := binary.BigEndian.Uint16(pkt[2:4])
	if e.hdr.Count == 0 {
		e.hdr.SNBase = seq
	} else if seq != e.hdr.SNBase+uint16(e.hdr.Count) {
		return ErrInvalid
	}
	if int(e.hdr.Count) >= MaxFECGroup {
		return ErrInvalid
	}
	ts := binary.BigEndian.Uint32(pkt[4:8])
	e.hdr.Count++
	e.hdr.Byte0Rec ^= pkt[0] & 0x3f
	e.hdr.Byte1Rec ^= pkt[1]
	e.hdr.LengthRec ^= uint16(len(body))
	e.hdr.TSRec ^= ts
	e.lastTS = ts
	if len(body) > e.maxBody {
		e.maxBody = len(body)
	}
	subtle.XORBytes(e.parity[:len(body)], e.parity[:len(body)], body)
	return nil
}

// Flush writes the parity packet for the current group into out, using ssrc and seq
// for its RTP header, and starts a new group. It returns the packet length.
func (e *FECEncoder) Flush(out []byte, ssrc uint32, seq uint16) (int, error) {
	if e.hdr.Count == 0 {
		return 0, ErrInvalid
	}
	n := RTPHeaderSize + FECHeaderSize + e.maxBody
	if len(out) < n {
		return 0, ErrBufferSize
	}
	h := RTPHeader{PayloadType: PTFEC, SequenceNumber: seq, Timestamp: e.lastTS, SSRC: ssrc}
	if _, err := h.MarshalTo(out); err != nil {
		return 0, err
	}
	if _, err := e.hdr.MarshalTo(out[RTPHeaderSize:]); err != nil {
		return 0, err
	}
	copy(out[RTPHeaderSize+FECHeaderSize:n], e.parity[:e.maxBody])
	e.reset()
	return n, nil
}

func (e *FECEncoder) reset() {
	clear(e.parity[:e.maxBody])
	e.hdr = FECHeader{}
	e.maxBody = 0
	e.lastTS = 0
}

// FECRecovery rebuilds the single missing packet of a group from its parity packet and
// the k-1 packets that arrived. Use Start, then Add for each received packet, then
// Finish. The zero value is ready to use and can be reused.
type FECRecovery struct {
	hdr       FECHeader
	ssrc      uint32
	parityLen int
	added     int
	body      [MaxProtectedBody]byte
}

// Start loads a parity packet (a complete RTP packet with PT 127).
func (r *FECRecovery) Start(parityPkt []byte) error {
	var h RTPHeader
	payload, err := ParseRTP(parityPkt, &h)
	if err != nil {
		return err
	}
	if h.PayloadType != PTFEC {
		return ErrInvalid
	}
	parity, err := ParseFEC(payload, &r.hdr)
	if err != nil {
		return err
	}
	r.ssrc = h.SSRC
	r.parityLen = len(parity)
	r.added = 0
	copy(r.body[:], parity)
	return nil
}

// Header returns the FEC header of the loaded parity packet.
func (r *FECRecovery) Header() FECHeader { return r.hdr }

// Add folds in one received packet of the group.
func (r *FECRecovery) Add(pkt []byte) error {
	if len(pkt) < RTPHeaderSize || !IsRTP(pkt) {
		return ErrShort
	}
	body := pkt[RTPHeaderSize:]
	if len(body) > r.parityLen {
		return ErrInvalid
	}
	if !r.hdr.Covers(binary.BigEndian.Uint16(pkt[2:4])) {
		return ErrInvalid
	}
	r.hdr.Byte0Rec ^= pkt[0] & 0x3f
	r.hdr.Byte1Rec ^= pkt[1]
	r.hdr.LengthRec ^= uint16(len(body))
	r.hdr.TSRec ^= binary.BigEndian.Uint32(pkt[4:8])
	subtle.XORBytes(r.body[:len(body)], r.body[:len(body)], body)
	r.added++
	return nil
}

// Finish writes the rebuilt packet with sequence number seq into out. It requires
// that exactly Count-1 packets were added.
func (r *FECRecovery) Finish(out []byte, seq uint16) (int, error) {
	if r.added != int(r.hdr.Count)-1 || !r.hdr.Covers(seq) {
		return 0, ErrInvalid
	}
	length := int(r.hdr.LengthRec)
	if length > r.parityLen {
		return 0, ErrInvalid
	}
	n := RTPHeaderSize + length
	if len(out) < n {
		return 0, ErrBufferSize
	}
	out[0] = RTPVersion<<6 | r.hdr.Byte0Rec&0x3f
	out[1] = r.hdr.Byte1Rec
	binary.BigEndian.PutUint16(out[2:4], seq)
	binary.BigEndian.PutUint32(out[4:8], r.hdr.TSRec)
	binary.BigEndian.PutUint32(out[8:12], r.ssrc)
	copy(out[RTPHeaderSize:n], r.body[:length])
	return n, nil
}
