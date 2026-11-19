package protocol

import "encoding/binary"

// RTCP generic NACK (RFC 4585, section 6.2.1): a receiver asks for the retransmission
// of video packets it is missing.
//
//	|V=2|P| FMT=1 |   PT=205    |          length               |
//	|                  SSRC of packet sender                      |
//	|                  SSRC of media source                       |
//	|            PID                |             BLP             |  (1-16 items)
//
// PID is a lost sequence number; bit i of BLP set means PID+i+1 is lost too. No RTP
// packet of this system has 205 in its second byte (payload types 96, 97 and 127,
// with or without the marker bit), so the two never collide.
const (
	RTCPTypeRTPFB  uint8 = 205
	NACKFormat     uint8 = 1
	NACKHeaderSize       = 12
	MaxNACKItems         = 16 // up to 272 sequence numbers in one NACK
)

// NACKItem is one PID/BLP pair.
type NACKItem struct {
	PID uint16
	BLP uint16
}

// NACK is a decoded generic NACK.
type NACK struct {
	SenderSSRC uint32 // the receiver that is missing packets
	MediaSSRC  uint32 // the stream they belong to
	Items      [MaxNACKItems]NACKItem
	N          int // items in use
}

// IsNACK reports whether b starts like an RTCP generic NACK.
func IsNACK(b []byte) bool {
	return len(b) >= 2 && b[0]>>6 == RTPVersion && b[0]&0x1f == NACKFormat && b[1] == RTCPTypeRTPFB
}

// ParseNACK decodes b into n. b must be exactly one NACK: no padding, no other RTCP
// packets after it, between 1 and MaxNACKItems items.
func ParseNACK(b []byte, n *NACK) error {
	if len(b) < NACKHeaderSize+4 {
		return ErrShort
	}
	if !IsNACK(b) {
		return ErrInvalid
	}
	if b[0]&0x20 != 0 {
		return ErrPadding
	}
	size := (int(binary.BigEndian.Uint16(b[2:4])) + 1) * 4
	if size > len(b) {
		return ErrShort
	}
	items := (size - NACKHeaderSize) / 4
	if size != len(b) || items < 1 || items > MaxNACKItems {
		return ErrInvalid
	}
	n.SenderSSRC = binary.BigEndian.Uint32(b[4:8])
	n.MediaSSRC = binary.BigEndian.Uint32(b[8:12])
	n.N = items
	for i := range items {
		off := NACKHeaderSize + 4*i
		n.Items[i] = NACKItem{PID: binary.BigEndian.Uint16(b[off:]), BLP: binary.BigEndian.Uint16(b[off+2:])}
	}
	return nil
}

// MarshalTo encodes n into buf and returns the number of bytes written.
func (n *NACK) MarshalTo(buf []byte) (int, error) {
	if n.N < 1 || n.N > MaxNACKItems {
		return 0, ErrInvalid
	}
	size := NACKHeaderSize + 4*n.N
	if len(buf) < size {
		return 0, ErrBufferSize
	}
	buf[0] = RTPVersion<<6 | NACKFormat
	buf[1] = RTCPTypeRTPFB
	binary.BigEndian.PutUint16(buf[2:4], uint16(size/4-1))
	binary.BigEndian.PutUint32(buf[4:8], n.SenderSSRC)
	binary.BigEndian.PutUint32(buf[8:12], n.MediaSSRC)
	for i, it := range n.Items[:n.N] {
		off := NACKHeaderSize + 4*i
		binary.BigEndian.PutUint16(buf[off:], it.PID)
		binary.BigEndian.PutUint16(buf[off+2:], it.BLP)
	}
	return size, nil
}

// SetLost packs lost sequence numbers, in increasing order (modulo 2^16), into n's
// items and returns how many of them fitted.
func (n *NACK) SetLost(lost []uint16) int {
	n.N = 0
	for i, seq := range lost {
		if n.N > 0 {
			it := &n.Items[n.N-1]
			if d := seq - it.PID; d >= 1 && d <= 16 {
				it.BLP |= 1 << (d - 1)
				continue
			}
		}
		if n.N == MaxNACKItems {
			return i
		}
		n.Items[n.N] = NACKItem{PID: seq}
		n.N++
	}
	return len(lost)
}

// AppendLost appends the sequence numbers n lists to dst, in order.
func (n *NACK) AppendLost(dst []uint16) []uint16 {
	for _, it := range n.Items[:n.N] {
		dst = append(dst, it.PID)
		for bit := range uint16(16) {
			if it.BLP&(1<<bit) != 0 {
				dst = append(dst, it.PID+bit+1)
			}
		}
	}
	return dst
}
