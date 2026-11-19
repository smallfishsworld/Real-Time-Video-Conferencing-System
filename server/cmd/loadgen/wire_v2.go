package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/client"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

const batch = 64 // datagrams per sendmmsg/recvmmsg call

// v2Participant is one member of a room on the v2 server.
type v2Participant struct {
	idx    int
	cfg    config
	clk    clock
	ctrl   *client.Control
	media  *client.Media
	pc     *ipv4.PacketConn
	joined client.Message
	server *net.UDPAddr
	st     *senderStats

	vseq, aseq uint16
	venc, aenc protocol.FECEncoder
	bufs       [batch][protocol.MaxSendDatagram]byte
	msgs       []ipv4.Message
	pcm        [protocol.AudioFrameBytes]byte

	ssrcToSender map[uint32]uint32 // video SSRC -> participant index (shared, read-only)
}

func setupV2(ctx context.Context, cfg config, clk clock, server netip.Addr, parts []participant, stats []*senderStats) error {
	ctrlAddr := netip.AddrPortFrom(server, uint16(cfg.Port)).String()
	local := "127.0.0.1:0"
	if !server.IsLoopback() {
		local = "0.0.0.0:0"
	}
	vps := make([]*v2Participant, len(parts))
	var roomID uint32
	for i := range parts {
		ctrl, err := client.Dial(ctx, ctrlAddr, fmt.Sprintf("lg%d", i))
		if err != nil {
			return err
		}
		var msg client.Message
		if i%cfg.RoomSize == 0 {
			msg, err = ctrl.Request(ctx, map[string]any{"type": protocol.TypeCreateRoom, "title": fmt.Sprintf("load-%d", i/cfg.RoomSize)})
			roomID = msg.RoomID
		} else {
			msg, err = ctrl.Request(ctx, map[string]any{"type": protocol.TypeJoinRoom, "room_id": roomID})
		}
		if err != nil {
			_ = ctrl.Close()
			return fmt.Errorf("participant %d: %w", i, err)
		}
		media, err := client.ListenMedia(local, netip.AddrPortFrom(server, uint16(msg.MediaPort)))
		if err != nil {
			_ = ctrl.Close()
			return err
		}
		_ = media.Conn.SetReadBuffer(4 << 20)
		_ = media.Conn.SetWriteBuffer(4 << 20)
		if _, err := media.Register(msg.Token, 200*time.Millisecond, 5*time.Second); err != nil {
			_ = ctrl.Close()
			_ = media.Close()
			return fmt.Errorf("participant %d: %w", i, err)
		}
		p := &v2Participant{
			idx:    i,
			cfg:    cfg,
			clk:    clk,
			ctrl:   ctrl,
			media:  media,
			pc:     ipv4.NewPacketConn(media.Conn),
			joined: msg,
			server: net.UDPAddrFromAddrPort(media.Server),
			st:     stats[i],
			msgs:   make([]ipv4.Message, batch),
		}
		for k := range p.pcm { // low-level noise rather than digital silence
			p.pcm[k] = byte(k * 7)
		}
		go func() {
			// Events are not needed; read them so the buffer never fills. Ping like the
			// real client does, or the server closes the connection after its 60 s idle
			// timeout and the participant leaves the room in the middle of a long run.
			ping := time.NewTicker(15 * time.Second)
			defer ping.Stop()
			for {
				select {
				case <-ctrl.Events():
				case <-ping.C:
					_ = ctrl.Send(map[string]any{"type": protocol.TypePing})
				case <-ctrl.Done():
					return
				}
			}
		}()
		vps[i] = p
		parts[i] = p
	}
	ssrcs := make(map[uint32]uint32, len(vps))
	for i, p := range vps {
		ssrcs[p.joined.VideoSSRC] = uint32(i)
	}
	for _, p := range vps {
		p.ssrcToSender = ssrcs
	}
	time.Sleep(500 * time.Millisecond) // let the rooms' mode decisions settle
	return nil
}

func (p *v2Participant) write(n int) error {
	for i := 0; i < n; i++ {
		p.msgs[i].Addr = p.server
	}
	sent := 0
	for sent < n {
		k, err := p.pc.WriteBatch(p.msgs[sent:n], 0)
		if err != nil {
			return err
		}
		sent += k
	}
	return nil
}

func (p *v2Participant) queue(k int, pkt []byte) {
	p.msgs[k].Buffers = [][]byte{pkt}
	p.st.packets.Add(1)
	p.st.bytes.Add(int64(len(pkt)))
}

func (p *v2Participant) sendVideo(frame []byte, t tag) error {
	count := protocol.FragmentCount(len(frame))
	ts := uint32(t.SentNS * 9 / 100000) // 90 kHz
	k := 0
	for i := 0; i < count; i++ {
		chunk := frame[i*protocol.MaxFragmentPayload : min(len(frame), (i+1)*protocol.MaxFragmentPayload)]
		b := p.bufs[k][:]
		h := protocol.RTPHeader{
			Marker:         i == count-1,
			PayloadType:    protocol.PTVideo,
			SequenceNumber: p.vseq,
			Timestamp:      ts,
			SSRC:           p.joined.VideoSSRC,
		}
		p.vseq++
		n, _ := h.MarshalTo(b)
		v := protocol.VideoHeader{FrameID: t.Seq, FragmentIndex: uint16(i), FragmentCount: uint16(count)}
		m, _ := v.MarshalTo(b[n:])
		c := copy(b[n+m:], chunk)
		pkt := b[:n+m+c]
		p.queue(k, pkt)
		k++
		if p.cfg.FECK > 0 {
			_ = p.venc.Add(pkt)
			if p.venc.Len() >= p.cfg.FECK || i == count-1 {
				fn, err := p.venc.Flush(p.bufs[k][:], p.joined.VideoSSRC, p.vseq)
				if err == nil {
					p.vseq++
					p.queue(k, p.bufs[k][:fn])
					k++
				}
			}
		}
		if k >= batch-1 {
			if err := p.write(k); err != nil {
				return err
			}
			k = 0
		}
	}
	return p.write(k)
}

func (p *v2Participant) sendAudio(seq uint32) error {
	b := p.bufs[0][:]
	h := protocol.RTPHeader{
		Marker:         seq == 0,
		PayloadType:    protocol.PTAudio,
		SequenceNumber: p.aseq,
		Timestamp:      seq * protocol.AudioSamplesPerFrame,
		SSRC:           p.joined.AudioSSRC,
	}
	p.aseq++
	n, _ := h.MarshalTo(b)
	c := copy(b[n:], p.pcm[:])
	pkt := b[:n+c]
	p.queue(0, pkt)
	k := 1
	if p.cfg.AudioFECK > 0 {
		_ = p.aenc.Add(pkt)
		if p.aenc.Len() >= p.cfg.AudioFECK {
			fn, err := p.aenc.Flush(p.bufs[1][:], p.joined.AudioSSRC, p.aseq)
			if err == nil {
				p.aseq++
				p.queue(1, p.bufs[1][:fn])
				k = 2
			}
		}
	}
	return p.write(k)
}

type frameState struct {
	fid    uint32
	need   uint16
	got    uint16
	bits   uint64
	tag    tag
	hasTag bool
	active bool
	done   bool
}

func (p *v2Participant) receive(ctx context.Context, rec *recorder) {
	ms := make([]ipv4.Message, batch)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, protocol.MaxDatagram)}
	}
	stop := context.AfterFunc(ctx, func() { _ = p.media.Conn.SetReadDeadline(time.Now()) })
	defer stop()
	frames := make(map[uint32]*frameState)
	mixer := p.joined.MixerSSRC
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
			b := m.Buffers[0][:m.N]
			if len(b) < protocol.RTPHeaderSize || b[0]>>6 != 2 {
				rec.otherPackets++
				continue
			}
			pt := b[1] & 0x7f
			ssrc := binary.BigEndian.Uint32(b[8:12])
			switch {
			case pt == protocol.PTVideo:
				sender, ok := p.ssrcToSender[ssrc]
				off := protocol.RTPHeaderSize + 4*int(b[0]&0x0f)
				if !ok || len(b) < off+protocol.VideoHeaderSize {
					rec.otherPackets++
					continue
				}
				rec.videoPackets++
				rec.videoBytes += int64(len(b))
				fid := binary.BigEndian.Uint32(b[off:])
				idx := binary.BigEndian.Uint16(b[off+4:])
				count := binary.BigEndian.Uint16(b[off+6:])
				st := frames[sender]
				if st == nil {
					st = &frameState{}
					frames[sender] = st
				}
				if !st.active || st.fid != fid {
					if st.active && !st.done {
						rec.incomplete++
					}
					*st = frameState{fid: fid, need: count, active: true}
				}
				if idx >= 64 || st.bits&(1<<idx) != 0 {
					continue
				}
				st.bits |= 1 << idx
				st.got++
				if idx == 0 {
					st.tag, st.hasTag = readTag(b[off+protocol.VideoHeaderSize:])
				}
				if st.got == st.need && !st.done {
					st.done = true
					if st.hasTag {
						rec.frame(st.tag, now)
					} else {
						rec.badTags++
					}
				}
			case pt == protocol.PTAudio && ssrc == mixer:
				rec.audioBytes += int64(len(b))
				if rec.inWindow(now) {
					rec.audioInWin++
				}
			default:
				rec.otherPackets++ // FEC parity (not used for repair here) and anything else
			}
		}
	}
}

func (p *v2Participant) controlLost() bool {
	select {
	case <-p.ctrl.Done():
		return true
	default:
		return false
	}
}

func (p *v2Participant) close() {
	_ = p.ctrl.Close()
	_ = p.media.Close()
}
