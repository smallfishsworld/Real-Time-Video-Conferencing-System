package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// The original system's media datagram is pickle.dumps((ID, kind, data)) with the
// default protocol 4 (kind 1 = JPEG frame, 2 = 1024-sample audio chunk). appendPickle
// produces the same bytes as CPython's pickler for data shorter than 64 KiB, which
// all corpus frames are (see TestPickleMatchesCPython).
func appendPickle(dst []byte, id, kind int, data []byte) []byte {
	var body [16]byte
	n := 0
	if id < 256 {
		body[0], body[1] = 'K', byte(id) // BININT1
		n = 2
	} else {
		body[0], body[1], body[2] = 'M', byte(id), byte(id>>8) // BININT2
		n = 3
	}
	body[n], body[n+1] = 'K', byte(kind)
	n += 2
	if len(data) < 256 {
		body[n], body[n+1] = 'C', byte(len(data)) // SHORT_BINBYTES
		n += 2
	} else {
		body[n] = 'B' // BINBYTES
		binary.LittleEndian.PutUint32(body[n+1:], uint32(len(data)))
		n += 5
	}
	frameLen := n + len(data) + 4
	dst = append(dst, 0x80, 0x04, 0x95) // PROTO 4, FRAME
	dst = binary.LittleEndian.AppendUint64(dst, uint64(frameLen))
	dst = append(dst, body[:n]...)
	dst = append(dst, data...)
	return append(dst, 0x94, 0x87, 0x94, '.') // MEMOIZE, TUPLE3, MEMOIZE, STOP
}

// parsePickle extracts (ID, kind, data) from a datagram built like appendPickle's.
func parsePickle(b []byte) (id, kind int, data []byte, ok bool) {
	if len(b) < 20 || b[0] != 0x80 || b[1] != 0x04 || b[2] != 0x95 {
		return 0, 0, nil, false
	}
	off := 11
	switch b[off] {
	case 'K':
		id = int(b[off+1])
		off += 2
	case 'M':
		id = int(b[off+1]) | int(b[off+2])<<8
		off += 3
	default:
		return 0, 0, nil, false
	}
	if b[off] != 'K' {
		return 0, 0, nil, false
	}
	kind = int(b[off+1])
	off += 2
	var n int
	switch b[off] {
	case 'C':
		n = int(b[off+1])
		off += 2
	case 'B':
		if len(b) < off+5 {
			return 0, 0, nil, false
		}
		n = int(binary.LittleEndian.Uint32(b[off+1:]))
		off += 5
	default:
		return 0, 0, nil, false
	}
	if off+n > len(b) {
		return 0, 0, nil, false
	}
	return id, kind, b[off : off+n], true
}

// legacyParticipant follows the original client's port scheme: user ID n binds TCP
// 20000+n for the conference connection and UDP 20000+100n for media, and sends media
// to the conference's relay at UDP 10000+100*conference.
type legacyParticipant struct {
	idx      int
	id       int
	cfg      config
	clk      clock
	mainConn net.Conn
	confConn net.Conn
	confDone chan struct{} // closed when the conference connection ends
	conn     *net.UDPConn
	pc       *ipv4.PacketConn
	relay    *net.UDPAddr
	st       *senderStats
	buf      []byte
	chunk    []byte
}

func rjust10(s string) []byte {
	return []byte(strings.Repeat(" ", max(0, 10-len(s))) + s)
}

func addrPlus(base netip.Addr, n int) netip.Addr {
	b := base.As4()
	v := binary.BigEndian.Uint32(b[:]) + uint32(n)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func setupLegacy(ctx context.Context, cfg config, clk clock, server netip.Addr, parts []participant, stats []*senderStats) error {
	base, err := netip.ParseAddr(cfg.LocalBase)
	if err != nil || !base.Is4() {
		return fmt.Errorf("-local-base must be an IPv4 address")
	}
	rooms := (len(parts) + cfg.RoomSize - 1) / cfg.RoomSize
	errs := make([]error, rooms)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // rooms set up concurrently
	for room := 0; room < rooms; room++ {
		wg.Add(1)
		go func(room int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			conf := cfg.ConfBase + room
			for j := 0; j < cfg.RoomSize && room*cfg.RoomSize+j < len(parts); j++ {
				i := room*cfg.RoomSize + j
				p, err := joinLegacy(ctx, cfg, server, addrPlus(base, i+1), i, conf, j == 0, stats[i])
				if err != nil {
					errs[room] = fmt.Errorf("participant %d: %w", i, err)
					return
				}
				p.clk = clk
				parts[i] = p
			}
		}(room)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	time.Sleep(time.Second) // the relay thread of each conference binds its socket asynchronously
	return nil
}

func joinLegacy(ctx context.Context, cfg config, server, ip netip.Addr, idx, conf int, create bool, st *senderStats) (*legacyParticipant, error) {
	id := idx + 1
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(20000+100*id))))
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(4 << 20)
	_ = conn.SetWriteBuffer(4 << 20)
	p := &legacyParticipant{
		idx:   idx,
		id:    id,
		cfg:   cfg,
		conn:  conn,
		pc:    ipv4.NewPacketConn(conn),
		relay: net.UDPAddrFromAddrPort(netip.AddrPortFrom(server, uint16(10000+100*conf))),
		st:    st,
		buf:   make([]byte, 0, 64<<10),
		chunk: make([]byte, 2048),
	}
	for k := range p.chunk {
		p.chunk[k] = byte(k * 7)
	}
	cmd := "join"
	if create {
		cmd = "create"
	}
	p.mainConn, err = requestMain(ctx, cfg, server, ip, cmd, id, conf)
	if err != nil {
		p.close()
		return nil, err
	}
	cd := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(20000+id))), Timeout: 5 * time.Second}
	p.confConn, err = cd.DialContext(ctx, "tcp", netip.AddrPortFrom(server, uint16(10000+conf)).String())
	if err != nil {
		p.close()
		return nil, err
	}
	p.confDone = make(chan struct{})
	go func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c)
		close(p.confDone)
	}(p.confConn)
	go func(c net.Conn) { _, _ = io.Copy(io.Discard, c) }(p.mainConn)
	return p, nil
}

// requestMain sends one create/join request to the MainServer and waits for its
// "200:" reply. The MainServer reads the command with recv(1024) and then each 10-byte
// field with its own recv(10), so the command must arrive alone: if the next field is
// already in the socket buffer when the command is read, the server cannot parse it
// and never answers. The pause after the command makes that unlikely; when it happens
// anyway (a busy server), the request is retried on a new connection.
func requestMain(ctx context.Context, cfg config, server, ip netip.Addr, cmd string, id, conf int) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		d := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, 0)), Timeout: 5 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(server, uint16(cfg.Port)).String())
		if err != nil {
			return nil, err
		}
		reply, err := mainExchange(conn, cmd, id, conf)
		if err == nil && strings.HasPrefix(reply, "200:") {
			return conn, nil
		}
		_ = conn.Close()
		if err == nil {
			return nil, fmt.Errorf("legacy server answered %q", reply)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no answer from the legacy server after 4 attempts: %w", lastErr)
}

func mainExchange(conn net.Conn, cmd string, id, conf int) (string, error) {
	pauses := []time.Duration{50 * time.Millisecond, 10 * time.Millisecond, 0}
	for i, f := range []string{cmd, strconv.Itoa(id), strconv.Itoa(conf)} {
		if _, err := conn.Write(rjust10(f)); err != nil {
			return "", err
		}
		time.Sleep(pauses[i])
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply := make([]byte, 1024)
	n, err := conn.Read(reply)
	if err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Time{})
	return string(reply[:n]), nil
}

func (p *legacyParticipant) send(kind int, data []byte) error {
	p.buf = appendPickle(p.buf[:0], p.id, kind, data)
	_, err := p.conn.WriteToUDP(p.buf, p.relay)
	if err == nil {
		p.st.packets.Add(1)
		p.st.bytes.Add(int64(len(p.buf)))
	}
	return err
}

func (p *legacyParticipant) sendVideo(frame []byte, _ tag) error { return p.send(1, frame) }

func (p *legacyParticipant) sendAudio(uint32) error { return p.send(2, p.chunk) }

func (p *legacyParticipant) receive(ctx context.Context, rec *recorder) {
	ms := make([]ipv4.Message, 8)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, 100000)} // recvfrom(100000) in the original
	}
	stop := context.AfterFunc(ctx, func() { _ = p.conn.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		n, err := p.pc.ReadBatch(ms, 0)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
				return
			}
			rec.readErrors++
			continue
		}
		now := p.clk.now()
		for _, m := range ms[:n] {
			_, kind, data, ok := parsePickle(m.Buffers[0][:m.N])
			switch {
			case !ok:
				rec.otherPackets++
			case kind == 1:
				rec.videoPackets++
				rec.videoBytes += int64(m.N)
				if t, ok := readTag(data); ok {
					rec.frame(t, now)
				} else {
					rec.badTags++
				}
			case kind == 2:
				rec.audioBytes += int64(m.N)
				if rec.inWindow(now) {
					rec.audioInWin++
				}
			default:
				rec.otherPackets++
			}
		}
	}
}

func (p *legacyParticipant) controlLost() bool {
	select {
	case <-p.confDone:
		return true
	default:
		return false
	}
}

func (p *legacyParticipant) close() {
	if p.mainConn != nil {
		_ = p.mainConn.Close()
	}
	if p.confConn != nil {
		_ = p.confConn.Close()
	}
	_ = p.conn.Close()
}
