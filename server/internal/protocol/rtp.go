// Package protocol implements the wire formats described in docs/PROTOCOL.md: the RTP
// fixed header, the video fragment header, XOR FEC parity, the UDP registration packets
// and the length-prefixed JSON control messages.
//
// Decoders never allocate and never retain the input buffer beyond the returned
// sub-slices, so they can be used on the media hot path with reused buffers.
package protocol

import (
	"encoding/binary"
	"errors"
)

// RTP constants (RFC 3550) and the payload types used by this system.
const (
	RTPVersion    = 2
	RTPHeaderSize = 12
	MaxCSRC       = 15

	PTVideo uint8 = 96
	PTAudio uint8 = 97
	PTFEC   uint8 = 127

	VideoClockRate = 90000
	AudioClockRate = 16000

	// AudioSamplesPerFrame is 20 ms of 16 kHz mono audio.
	AudioSamplesPerFrame = 320
	// AudioFrameBytes is the L16 payload size of one audio frame.
	AudioFrameBytes = AudioSamplesPerFrame * 2

	// MaxDatagram is the largest datagram the server accepts.
	MaxDatagram = 1500
	// MaxSendDatagram is the largest datagram any sender in this system produces
	// (RTP header + FEC header + video header + fragment), which fits a 1500-byte
	// MTU path without IP fragmentation for IPv4 and IPv6.
	MaxSendDatagram = RTPHeaderSize + FECHeaderSize + VideoHeaderSize + MaxFragmentPayload
)

// Errors returned by the decoders.
var (
	ErrShort      = errors.New("protocol: packet too short")
	ErrVersion    = errors.New("protocol: unsupported RTP version")
	ErrPadding    = errors.New("protocol: invalid RTP padding")
	ErrBufferSize = errors.New("protocol: output buffer too small")
	ErrInvalid    = errors.New("protocol: invalid field value")
)

// RTPHeader is a decoded RTP fixed header plus its CSRC list.
type RTPHeader struct {
	Padding        bool
	Extension      bool
	Marker         bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	CSRCCount      uint8
	CSRC           [MaxCSRC]uint32
}

// IsRTP reports whether the first byte of b looks like an RTP version 2 packet.
func IsRTP(b []byte) bool {
	return len(b) > 0 && b[0]>>6 == RTPVersion
}

// ParseRTP decodes the RTP header of pkt into h and returns the payload, which aliases
// pkt. Padding is removed and a header extension, if present, is skipped.
func ParseRTP(pkt []byte, h *RTPHeader) ([]byte, error) {
	if len(pkt) < RTPHeaderSize {
		return nil, ErrShort
	}
	b0, b1 := pkt[0], pkt[1]
	if b0>>6 != RTPVersion {
		return nil, ErrVersion
	}
	h.Padding = b0&0x20 != 0
	h.Extension = b0&0x10 != 0
	h.CSRCCount = b0 & 0x0f
	h.Marker = b1&0x80 != 0
	h.PayloadType = b1 & 0x7f
	h.SequenceNumber = binary.BigEndian.Uint16(pkt[2:4])
	h.Timestamp = binary.BigEndian.Uint32(pkt[4:8])
	h.SSRC = binary.BigEndian.Uint32(pkt[8:12])

	off := RTPHeaderSize
	cc := int(h.CSRCCount)
	if len(pkt) < off+4*cc {
		return nil, ErrShort
	}
	for i := 0; i < cc; i++ {
		h.CSRC[i] = binary.BigEndian.Uint32(pkt[off : off+4])
		off += 4
	}
	if h.Extension {
		if len(pkt) < off+4 {
			return nil, ErrShort
		}
		extWords := int(binary.BigEndian.Uint16(pkt[off+2 : off+4]))
		off += 4 + 4*extWords
		if len(pkt) < off {
			return nil, ErrShort
		}
	}
	end := len(pkt)
	if h.Padding {
		pad := int(pkt[end-1])
		if pad == 0 || end-pad < off {
			return nil, ErrPadding
		}
		end -= pad
	}
	return pkt[off:end], nil
}

// HeaderSize returns the encoded size of h (fixed header plus CSRC list). Encoded
// headers never carry an extension.
func (h *RTPHeader) HeaderSize() int {
	return RTPHeaderSize + 4*int(h.CSRCCount)
}

// MarshalTo writes the header into buf and returns the number of bytes written.
// Padding and Extension are not supported by the encoder and must be false.
func (h *RTPHeader) MarshalTo(buf []byte) (int, error) {
	if h.CSRCCount > MaxCSRC || h.PayloadType > 0x7f || h.Padding || h.Extension {
		return 0, ErrInvalid
	}
	n := h.HeaderSize()
	if len(buf) < n {
		return 0, ErrBufferSize
	}
	buf[0] = RTPVersion<<6 | h.CSRCCount
	buf[1] = h.PayloadType
	if h.Marker {
		buf[1] |= 0x80
	}
	binary.BigEndian.PutUint16(buf[2:4], h.SequenceNumber)
	binary.BigEndian.PutUint32(buf[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(buf[8:12], h.SSRC)
	off := RTPHeaderSize
	for i := 0; i < int(h.CSRCCount); i++ {
		binary.BigEndian.PutUint32(buf[off:off+4], h.CSRC[i])
		off += 4
	}
	return n, nil
}

// PeekSSRC returns the SSRC and payload type of an RTP packet without a full parse.
// The caller must have checked IsRTP and len(pkt) >= RTPHeaderSize.
func PeekSSRC(pkt []byte) (ssrc uint32, pt uint8) {
	return binary.BigEndian.Uint32(pkt[8:12]), pkt[1] & 0x7f
}

// SeqDiff returns the signed distance a-b in sequence-number space (RFC 3550 style
// wrap-around arithmetic).
func SeqDiff(a, b uint16) int {
	return int(int16(a - b))
}
