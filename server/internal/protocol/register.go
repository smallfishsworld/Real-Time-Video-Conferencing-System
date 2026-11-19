package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
)

// Registration packet types and sizes (docs/PROTOCOL.md section 2.2).
const (
	TypeRegister    byte = 0x01
	TypeRegisterAck byte = 0x02

	RegisterVersion = 1
	RegisterSize    = 28
	TokenSize       = 16
)

// Token is the per-participant secret handed out in the `joined` message.
type Token [TokenSize]byte

// String returns the token as 32 lower-case hex characters.
func (t Token) String() string { return hex.EncodeToString(t[:]) }

// ParseToken decodes 32 hex characters into a Token.
func ParseToken(s string) (Token, error) {
	var t Token
	if hex.DecodedLen(len(s)) != TokenSize {
		return t, ErrInvalid
	}
	if _, err := hex.Decode(t[:], []byte(s)); err != nil {
		return t, ErrInvalid
	}
	return t, nil
}

// Register is the client's address registration (binding request).
type Register struct {
	TxID  uint64
	Token Token
}

// ParseRegister decodes a REGISTER packet.
func ParseRegister(b []byte, r *Register) error {
	if len(b) < RegisterSize {
		return ErrShort
	}
	if b[0] != TypeRegister || b[1] != RegisterVersion {
		return ErrInvalid
	}
	r.TxID = binary.BigEndian.Uint64(b[4:12])
	copy(r.Token[:], b[12:28])
	return nil
}

// MarshalTo writes the REGISTER packet into buf.
func (r *Register) MarshalTo(buf []byte) (int, error) {
	if len(buf) < RegisterSize {
		return 0, ErrBufferSize
	}
	buf[0] = TypeRegister
	buf[1] = RegisterVersion
	buf[2], buf[3] = 0, 0
	binary.BigEndian.PutUint64(buf[4:12], r.TxID)
	copy(buf[12:28], r.Token[:])
	return RegisterSize, nil
}

// RegisterAck is the server's answer to a valid REGISTER.
type RegisterAck struct {
	TxID     uint64
	Observed netip.AddrPort
}

// MarshalTo writes the REGISTER_ACK packet into buf.
func (a *RegisterAck) MarshalTo(buf []byte) (int, error) {
	addr := a.Observed.Addr().Unmap()
	family, alen := byte(4), 4
	if addr.Is6() {
		family, alen = 6, 16
	} else if !addr.Is4() {
		return 0, ErrInvalid
	}
	n := 14 + alen
	if len(buf) < n {
		return 0, ErrBufferSize
	}
	buf[0] = TypeRegisterAck
	buf[1] = RegisterVersion
	buf[2] = family
	buf[3] = 0
	binary.BigEndian.PutUint64(buf[4:12], a.TxID)
	binary.BigEndian.PutUint16(buf[12:14], a.Observed.Port())
	if alen == 4 {
		a4 := addr.As4()
		copy(buf[14:18], a4[:])
	} else {
		a16 := addr.As16()
		copy(buf[14:30], a16[:])
	}
	return n, nil
}

// ParseRegisterAck decodes a REGISTER_ACK packet.
func ParseRegisterAck(b []byte, a *RegisterAck) error {
	if len(b) < 14 {
		return ErrShort
	}
	if b[0] != TypeRegisterAck || b[1] != RegisterVersion {
		return ErrInvalid
	}
	a.TxID = binary.BigEndian.Uint64(b[4:12])
	port := binary.BigEndian.Uint16(b[12:14])
	switch b[2] {
	case 4:
		if len(b) < 18 {
			return ErrShort
		}
		a.Observed = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[14:18])), port)
	case 6:
		if len(b) < 30 {
			return ErrShort
		}
		a.Observed = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[14:30])), port)
	default:
		return ErrInvalid
	}
	return nil
}
