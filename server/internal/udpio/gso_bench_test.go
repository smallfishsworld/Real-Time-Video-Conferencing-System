package udpio

import "testing"

// BenchmarkSendFrame sends one video frame's worth of full-size datagrams (24 x 1220
// bytes, about what the server forwards per frame and receiver) to one loopback
// receiver: one sendto per datagram, one sendmmsg for all of them, and one sendmmsg
// whose single message carries all 24 as UDP GSO segments. The receiver is not read,
// so the kernel drops datagrams once its buffer is full.
//
// Loopback only: the receive path runs in softirq on the sending CPU and is part of
// the measured time, and on lo (default gso_max_segs) a GSO message goes up that
// path whole and is split only at the receiving socket, so the GSO variant also pays
// less receive work per datagram than the others. A server on a real NIC would not do
// that receive work at all. bench/run_b3.sh sets lo gso_max_segs 1 to avoid this;
// this microbenchmark does not, so its GSO speed-up is an upper bound.
func BenchmarkSendFrame(b *testing.B) {
	const segs, size = 24, 1220
	frame := make([]byte, segs*size)
	bufs := make([][]byte, segs)
	for i := range bufs {
		bufs[i] = frame[i*size : (i+1)*size]
	}
	for _, bc := range []struct {
		name string
		opt  Options
	}{{"sendto", Options{}}, {"sendmmsg", Options{Batching: true}}, {"sendmmsg+gso", Options{Batching: true, GSO: true}}} {
		b.Run(bc.name, func(b *testing.B) {
			c := New(listen(b, "udp4", "127.0.0.1:0"), bc.opt)
			if bc.opt.Batching && !c.Batching() || bc.opt.GSO && !c.GSO() {
				b.Skip("not supported here")
			}
			d := c.Dest(addrOf(listen(b, "udp4", "127.0.0.1:0")))
			w := c.NewWriter()
			b.SetBytes(segs * size)
			b.ReportAllocs()
			for b.Loop() {
				for _, p := range bufs {
					w.Add(p, &d, 0)
				}
				if res := w.Flush(); res.Failed != 0 {
					b.Fatal(res)
				}
			}
		})
	}
}
