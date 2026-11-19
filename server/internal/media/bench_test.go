package media

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/udpio"
)

// discardWriter is a packetWriter that drops everything, so a benchmark measures the
// mixer's own work without system calls.
type discardWriter struct{ n int }

func (d *discardWriter) WriteToUDPAddrPort(b []byte, _ netip.AddrPort) (int, error) {
	d.n++
	return len(b), nil
}

// upstreamAudio is a reusable upstream audio packet whose sequence number and
// timestamp are patched in place.
type upstreamAudio struct {
	pkt []byte
	seq uint16
	ts  uint32
}

func newUpstreamAudio(ssrc uint32, v int16) *upstreamAudio {
	return &upstreamAudio{pkt: audioPacket(0, 0, ssrc, v)}
}

func (u *upstreamAudio) push(b *testing.B, jb *jitterBuffer) {
	binary.BigEndian.PutUint16(u.pkt[2:4], u.seq)
	binary.BigEndian.PutUint32(u.pkt[4:8], u.ts)
	u.seq++
	u.ts += frameSamples
	var h protocol.RTPHeader
	payload, err := protocol.ParseRTP(u.pkt, &h)
	if err != nil {
		b.Fatal(err)
	}
	if r := jb.push(u.pkt, &h, payload); r != pushAccepted {
		b.Fatalf("push result %d", r)
	}
}

// BenchmarkMixerTick measures one 20 ms mixer tick of a room in which every member
// talks: each member's upstream packet enters its jitter buffer, then the tick pops,
// mixes minus-one, packetises (with k=2 audio FEC) and "sends" one stream per member.
func BenchmarkMixerTick(b *testing.B) {
	for _, members := range []int{3, 4, 5, 16} {
		b.Run(fmt.Sprintf("members=%d", members), func(b *testing.B) {
			var stats Stats
			w := &discardWriter{}
			m := newMixRoom(1, 0xAA, w, &stats, &Hooks{}, 2, 0)
			rv := &room.RoomView{ID: 1, Mode: room.ModeRelay, MixerSSRC: 0xAA}
			snap := &mixSnapshot{room: rv}
			ups := make([]*upstreamAudio, members)
			for i := range members {
				p := &participant{
					id:        uint32(i + 1),
					audioSSRC: uint32(100 + i),
					jb:        newJitterBuffer(0, &stats),
					down:      downstream{silent: true},
				}
				addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(30000+i))
				snap.members = append(snap.members, mixMember{p: p, addr: addr})
				ups[i] = newUpstreamAudio(p.audioSSRC, int16(100*(i+1)))
			}
			m.snap.Store(snap)
			now := time.Now()
			b.ReportAllocs()
			for b.Loop() {
				for i, mm := range snap.members {
					ups[i].push(b, mm.p.jb)
				}
				m.tick(now)
			}
			if w.n == 0 {
				b.Fatal("the mixer sent nothing")
			}
			b.ReportMetric(float64(w.n)/float64(b.N), "packets/op")
		})
	}
}

// BenchmarkMixMinus is the arithmetic core of a 4-member tick: sum four frames once,
// then subtract each receiver's own frame with saturation.
func BenchmarkMixMinus(b *testing.B) {
	frames := make([]audioFrame, 4)
	for i := range frames {
		for j := range frames[i] {
			frames[i][j] = int16((i + 1) * (j - 160) * 37)
		}
	}
	var total [frameSamples]int32
	var out audioFrame
	b.ReportAllocs()
	for b.Loop() {
		total = [frameSamples]int32{}
		for i := range frames {
			accumulate(&total, &frames[i])
		}
		for i := range frames {
			mixMinus(&out, &total, &frames[i])
		}
	}
}

// BenchmarkForwardVideo measures the server's per-packet video path with real
// loopback sockets: lookup, validation, rate limiting and the copies for the other
// three members of a four-member room (full-size fragments of one sender).
// "unbatched" sends each copy with its own sendto, as the server did before batching;
// "sendmmsg/batch=N" collects N received packets and sends their copies with one
// sendmmsg, as Serve does when a read returns N datagrams; "gso/batch=N" also merges
// each receiver's copies into UDP GSO messages. The receiving sockets are not read,
// so the kernel drops the copies once their buffers fill; the send side's cost is
// unaffected.
func BenchmarkForwardVideo(b *testing.B) {
	for _, bc := range []struct {
		name         string
		batch, flush int
		gso          bool
	}{
		{"unbatched", 1, 1, false},
		{"sendmmsg/batch=1", 64, 1, false}, {"sendmmsg/batch=16", 64, 16, false}, {"sendmmsg/batch=64", 64, 64, false},
		{"gso/batch=16", 64, 16, true}, {"gso/batch=64", 64, 64, true},
	} {
		b.Run(bc.name, func(b *testing.B) { benchmarkForwardVideo(b, bc.batch, bc.flush, bc.gso) })
	}
}

func benchmarkForwardVideo(b *testing.B, batch, flushEvery int, gso bool) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	r := NewRouter(conn, nil, Config{UDPBatch: batch, UDPGSO: gso}, Hooks{})
	targets := make([]udpio.Dest, 3)
	for i := range targets {
		sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			b.Fatal(err)
		}
		defer sink.Close()
		targets[i] = r.io.Dest(sink.LocalAddr().(*net.UDPAddr).AddrPort())
	}
	src := netip.MustParseAddrPort("127.0.0.1:40000")
	rv := &room.RoomView{ID: 1, Mode: room.ModeRelay}
	p := &participant{id: 1, audioSSRC: 10, bucket: newTokenBucket(0, 0)}
	r.table.Store(&routeTable{bySSRC: map[uint32]*route{
		11: {p: p, kind: kindVideo, addr: src, room: rv, targets: targets},
	}})
	pkt := make([]byte, protocol.RTPHeaderSize+protocol.VideoHeaderSize+protocol.MaxFragmentPayload)
	h := protocol.RTPHeader{PayloadType: protocol.PTVideo, SequenceNumber: 1, Timestamp: 90000, SSRC: 11}
	n, _ := h.MarshalTo(pkt)
	v := protocol.VideoHeader{FrameID: 1, FragmentIndex: 0, FragmentCount: 26}
	_, _ = v.MarshalTo(pkt[n:])
	b.SetBytes(int64(len(pkt) * len(targets)))
	b.ReportAllocs()
	now := time.Now()
	i := 0
	for b.Loop() {
		r.handle(pkt, src, now)
		if i++; i == flushEvery {
			r.flush(now)
			i = 0
		}
	}
	r.flush(now)
	b.StopTimer()
	if got := r.stats.PacketsOut[OutVideo].Load(); got < uint64(b.N*len(targets)) {
		b.Fatalf("forwarded %d copies, want %d", got, b.N*len(targets))
	}
}
