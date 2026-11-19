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

	// RegisterVersion 2 added the return-routability nonce; version 1 packets are
	// rejected, so an old client fails to register instead of mistaking a challenge
	// for a binding.
	RegisterVersion = 2
	RegisterSize    = 36
	TokenSize       = 16
	NonceSize       = 8

	// RegisterFlagNonce: the REGISTER echoes the nonce of a challenge.
	RegisterFlagNonce byte = 0x01
	// AckFlagChallenge: the REGISTER_ACK is a challenge; the binding has not moved yet.
	AckFlagChallenge byte = 0x01
)

// Nonce is the server's return-routability challenge for one token and address.
type Nonce [NonceSize]byte

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

// Register is the client's address registration (binding request). HasNonce is set
// when the packet echoes the nonce of a challenge REGISTER_ACK.
type Register struct {
	TxID     uint64
	Token    Token
	HasNonce bool
	Nonce    Nonce
}

// ParseRegister decodes a REGISTER packet.
func ParseRegister(b []byte, r *Register) error {
	if len(b) < RegisterSize {
		return ErrShort
	}
	if b[0] != TypeRegister || b[1] != RegisterVersion || b[2]&^RegisterFlagNonce != 0 {
		return ErrInvalid
	}
	r.TxID = binary.BigEndian.Uint64(b[4:12])
	copy(r.Token[:], b[12:28])
	r.HasNonce = b[2]&RegisterFlagNonce != 0
	if r.HasNonce {
		copy(r.Nonce[:], b[28:36])
	} else {
		r.Nonce = Nonce{}
	}
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
	clear(buf[28:36])
	if r.HasNonce {
		buf[2] = RegisterFlagNonce
		copy(buf[28:36], r.Nonce[:])
	}
	return RegisterSize, nil
}

// RegisterAck is the server's answer to a valid REGISTER. With Challenge set, the
// binding has not been made or moved: the client must send REGISTER again from the
// same address, echoing Nonce.
type RegisterAck struct {
	TxID      uint64
	Observed  netip.AddrPort
	Challenge bool
	Nonce     Nonce
}

// RegisterAckSize returns the size of a REGISTER_ACK for an IPv4 (false) or IPv6
// (true) address.
func RegisterAckSize(ipv6 bool) int {
	if ipv6 {
		return 14 + 16 + NonceSize
	}
	return 14 + 4 + NonceSize
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
	n := 14 + alen + NonceSize
	if len(buf) < n {
		return 0, ErrBufferSize
	}
	buf[0] = TypeRegisterAck
	buf[1] = RegisterVersion
	buf[2] = family
	buf[3] = 0
	if a.Challenge {
		buf[3] = AckFlagChallenge
	}
	binary.BigEndian.PutUint64(buf[4:12], a.TxID)
	binary.BigEndian.PutUint16(buf[12:14], a.Observed.Port())
	if alen == 4 {
		a4 := addr.As4()
		copy(buf[14:18], a4[:])
	} else {
		a16 := addr.As16()
		copy(buf[14:30], a16[:])
	}
	copy(buf[14+alen:n], a.Nonce[:])
	return n, nil
}

// ParseRegisterAck decodes a REGISTER_ACK packet.
func ParseRegisterAck(b []byte, a *RegisterAck) error {
	if len(b) < 14 {
		return ErrShort
	}
	if b[0] != TypeRegisterAck || b[1] != RegisterVersion || b[3]&^AckFlagChallenge != 0 {
		return ErrInvalid
	}
	a.TxID = binary.BigEndian.Uint64(b[4:12])
	port := binary.BigEndian.Uint16(b[12:14])
	var alen int
	switch b[2] {
	case 4:
		alen = 4
		if len(b) < 14+alen+NonceSize {
			return ErrShort
		}
		a.Observed = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[14:18])), port)
	case 6:
		alen = 16
		if len(b) < 14+alen+NonceSize {
			return ErrShort
		}
		a.Observed = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[14:30])), port)
	default:
		return ErrInvalid
	}
	a.Challenge = b[3]&AckFlagChallenge != 0
	copy(a.Nonce[:], b[14+alen:14+alen+NonceSize])
	return nil
}
