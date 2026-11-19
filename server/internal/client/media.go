package client

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// Media is a participant's UDP socket.
type Media struct {
	Conn   *net.UDPConn
	Server netip.AddrPort
}

// ListenMedia opens a UDP socket on local (for example "127.0.0.1:0") that talks to
// the media server at server.
func ListenMedia(local string, server netip.AddrPort) (*Media, error) {
	addr, err := net.ResolveUDPAddr("udp", local)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	return &Media{Conn: conn, Server: server}, nil
}

// LocalAddr returns the socket's address.
func (m *Media) LocalAddr() netip.AddrPort {
	ap := m.Conn.LocalAddr().(*net.UDPAddr).AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// Close closes the socket.
func (m *Media) Close() error { return m.Conn.Close() }

// ErrNoAck is returned when the server never acknowledged the registration.
var ErrNoAck = errors.New("client: no REGISTER_ACK")

// Register sends REGISTER every interval until the server binds the socket or timeout
// passes. The server first answers with a challenge; Register echoes its nonce
// (return routability) and waits for the final REGISTER_ACK. Other datagrams that
// arrive meanwhile are discarded.
func (m *Media) Register(token string, interval, timeout time.Duration) (netip.AddrPort, error) {
	tok, err := protocol.ParseToken(token)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var txb [8]byte
	_, _ = rand.Read(txb[:])
	reg := protocol.Register{TxID: binary.BigEndian.Uint64(txb[:]), Token: tok}
	pkt := make([]byte, protocol.RegisterSize)
	buf := make([]byte, 2048)
	deadline := time.Now().Add(timeout)
	defer func() { _ = m.Conn.SetReadDeadline(time.Time{}) }()
	for time.Now().Before(deadline) {
		n, _ := reg.MarshalTo(pkt)
		if _, err := m.Conn.WriteToUDPAddrPort(pkt[:n], m.Server); err != nil {
			return netip.AddrPort{}, err
		}
		wait := time.Now().Add(interval)
		for {
			_ = m.Conn.SetReadDeadline(wait)
			k, _, err := m.Conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				break
			}
			var ack protocol.RegisterAck
			if protocol.ParseRegisterAck(buf[:k], &ack) != nil || ack.TxID != reg.TxID {
				continue
			}
			if !ack.Challenge {
				return ack.Observed, nil
			}
			reg.HasNonce, reg.Nonce = true, ack.Nonce
			break // answer the challenge right away
		}
	}
	return netip.AddrPort{}, ErrNoAck
}

// Keepalive sends one REGISTER for token, as clients do every 5 s to keep the binding
// (the server drops bindings that stop sending them). It does not wait for the ACK.
func (m *Media) Keepalive(token string) error {
	tok, err := protocol.ParseToken(token)
	if err != nil {
		return err
	}
	var txb [8]byte
	_, _ = rand.Read(txb[:])
	reg := protocol.Register{TxID: binary.BigEndian.Uint64(txb[:]), Token: tok}
	pkt := make([]byte, protocol.RegisterSize)
	n, _ := reg.MarshalTo(pkt)
	_, err = m.Conn.WriteToUDPAddrPort(pkt[:n], m.Server)
	return err
}

// Packetizer builds RTP packets for one SSRC. The test clients and the load generator
// share it, so what the benchmarks send is what the tests check.
type Packetizer struct {
	SSRC uint32
	Seq  uint16
}

func newBuffer() []byte { return make([]byte, protocol.MaxSendDatagram) }

// VideoFrame appends the RTP packets of one JPEG frame (fragments of at most
// MaxFragmentPayload bytes) to out and returns it. With fecK > 0 a parity packet
// follows every fecK fragments and the last, possibly shorter, group of the frame.
func (p *Packetizer) VideoFrame(out [][]byte, frame []byte, frameID, ts uint32, fecK int, enc *protocol.FECEncoder) [][]byte {
	_ = p.VideoFrameTo(frame, frameID, ts, fecK, enc, newBuffer, func(pkt []byte) error {
		out = append(out, pkt)
		return nil
	})
	return out
}

// VideoFrameTo is VideoFrame without allocation: it builds each packet in the buffer
// that next returns (at least MaxSendDatagram bytes) and passes it to emit, which may
// use the packet until it lets next hand out that buffer again. It stops at emit's
// first error.
func (p *Packetizer) VideoFrameTo(frame []byte, frameID, ts uint32, fecK int, enc *protocol.FECEncoder,
	next func() []byte, emit func([]byte) error) error {
	count := protocol.FragmentCount(len(frame))
	for i := 0; i < count; i++ {
		chunk := frame[i*protocol.MaxFragmentPayload : min(len(frame), (i+1)*protocol.MaxFragmentPayload)]
		h := protocol.RTPHeader{
			Marker:         i == count-1,
			PayloadType:    protocol.PTVideo,
			SequenceNumber: p.Seq,
			Timestamp:      ts,
			SSRC:           p.SSRC,
		}
		p.Seq++
		b := next()
		n, _ := h.MarshalTo(b)
		v := protocol.VideoHeader{FrameID: frameID, FragmentIndex: uint16(i), FragmentCount: uint16(count)}
		m, _ := v.MarshalTo(b[n:])
		c := copy(b[n+m:], chunk)
		pkt := b[:n+m+c]
		if fecK > 0 {
			protect(enc, pkt)
		}
		if err := emit(pkt); err != nil {
			return err
		}
		if fecK > 0 && (enc.Len() >= fecK || i == count-1) {
			if err := p.flushParity(enc, next, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// AudioFrame returns the RTP packet of one 20 ms L16 frame (pcm is big-endian), plus a
// parity packet when fecK > 0 completes a group.
func (p *Packetizer) AudioFrame(out [][]byte, pcm []byte, ts uint32, marker bool, fecK int, enc *protocol.FECEncoder) [][]byte {
	_ = p.AudioFrameTo(pcm, ts, marker, fecK, enc, newBuffer, func(pkt []byte) error {
		out = append(out, pkt)
		return nil
	})
	return out
}

// AudioFrameTo is AudioFrame without allocation, like VideoFrameTo.
func (p *Packetizer) AudioFrameTo(pcm []byte, ts uint32, marker bool, fecK int, enc *protocol.FECEncoder,
	next func() []byte, emit func([]byte) error) error {
	h := protocol.RTPHeader{Marker: marker, PayloadType: protocol.PTAudio, SequenceNumber: p.Seq, Timestamp: ts, SSRC: p.SSRC}
	p.Seq++
	b := next()
	n, _ := h.MarshalTo(b)
	c := copy(b[n:], pcm)
	pkt := b[:n+c]
	if fecK > 0 {
		protect(enc, pkt)
	}
	if err := emit(pkt); err != nil {
		return err
	}
	if fecK > 0 && enc.Len() >= fecK {
		return p.flushParity(enc, next, emit)
	}
	return nil
}

// protect adds pkt to the current FEC group. A packet the group cannot take (a
// sequence gap, a full group) starts a new group, so one bad group cannot switch the
// stream's FEC off for good.
func protect(enc *protocol.FECEncoder, pkt []byte) {
	if enc.Add(pkt) != nil {
		enc.Reset()
		_ = enc.Add(pkt)
	}
}

// flushParity closes the current FEC group and emits its parity packet.
func (p *Packetizer) flushParity(enc *protocol.FECEncoder, next func() []byte, emit func([]byte) error) error {
	b := next()
	n, err := enc.Flush(b, p.SSRC, p.Seq)
	if err != nil {
		return nil // an empty group: nothing to send
	}
	p.Seq++
	return emit(b[:n])
}
