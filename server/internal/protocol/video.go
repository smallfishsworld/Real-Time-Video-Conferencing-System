package protocol

import "encoding/binary"

// Video fragmentation limits (docs/PROTOCOL.md section 2.4).
const (
	VideoHeaderSize    = 8
	MaxFragmentPayload = 1200
	MaxFragments       = 1024
	MaxFrameBytes      = MaxFragments * MaxFragmentPayload
)

// VideoHeader is the payload header in front of every JPEG fragment.
type VideoHeader struct {
	FrameID       uint32
	FragmentIndex uint16
	FragmentCount uint16
}

// ParseVideo decodes the video payload header and returns the fragment bytes.
func ParseVideo(payload []byte, v *VideoHeader) ([]byte, error) {
	if len(payload) < VideoHeaderSize+1 {
		return nil, ErrShort
	}
	v.FrameID = binary.BigEndian.Uint32(payload[0:4])
	v.FragmentIndex = binary.BigEndian.Uint16(payload[4:6])
	v.FragmentCount = binary.BigEndian.Uint16(payload[6:8])
	if v.FragmentCount == 0 || v.FragmentCount > MaxFragments || v.FragmentIndex >= v.FragmentCount {
		return nil, ErrInvalid
	}
	frag := payload[VideoHeaderSize:]
	if len(frag) > MaxFragmentPayload {
		return nil, ErrInvalid
	}
	return frag, nil
}

// MarshalTo writes the video header into buf.
func (v *VideoHeader) MarshalTo(buf []byte) (int, error) {
	if len(buf) < VideoHeaderSize {
		return 0, ErrBufferSize
	}
	if v.FragmentCount == 0 || v.FragmentCount > MaxFragments || v.FragmentIndex >= v.FragmentCount {
		return 0, ErrInvalid
	}
	binary.BigEndian.PutUint32(buf[0:4], v.FrameID)
	binary.BigEndian.PutUint16(buf[4:6], v.FragmentIndex)
	binary.BigEndian.PutUint16(buf[6:8], v.FragmentCount)
	return VideoHeaderSize, nil
}

// FragmentCount returns how many fragments a frame of frameLen bytes needs.
func FragmentCount(frameLen int) int {
	return (frameLen + MaxFragmentPayload - 1) / MaxFragmentPayload
}
