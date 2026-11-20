// Package udpio reads and writes UDP datagrams in batches.
//
// On 64-bit Linux (amd64, arm64) a Conn reads with recvmmsg(2) and a Writer sends with
// sendmmsg(2), so a burst of datagrams costs one system call in each direction instead
// of one per datagram. With UDP generic segmentation offload (GSO) enabled, a Writer
// also sends consecutive datagrams for the same destination as one message carrying
// UDP_SEGMENT, which the kernel splits into the original datagrams as late as possible
// (on a real NIC, in hardware or just before the driver), so the IP stack runs once
// per message rather than once per datagram. Elsewhere, or with batching turned off,
// the same API falls back to one ReadFromUDPAddrPort or WriteToUDPAddrPort call per
// datagram, which is what the media server did before batching. Neither path
// allocates once its arrays have grown to the working size.
package udpio

import (
	"net"
	"net/netip"
	"sync/atomic"
)

// MaxTags is the number of tags a Writer counts separately in a Result.
const MaxTags = 8

const (
	// maxVLen is the most messages one recvmmsg or sendmmsg call accepts (the
	// kernel's UIO_MAXIOV); larger batches are split into several calls.
	maxVLen = 1024
	// maxSegments and maxGSOBytes bound one GSO message: the kernel's
	// UDP_MAX_SEGMENTS (64 before Linux 6.10) and the 64 KiB IP datagram limit, with
	// room for headers.
	maxSegments = 64
	maxGSOBytes = 65000
)

// Options selects the system calls a Conn uses.
type Options struct {
	Batching bool // recvmmsg and sendmmsg
	GSO      bool // UDP GSO for consecutive datagrams to one destination (needs Batching)
	// OnGSOOff, if set, is called once when the kernel refuses GSO and the Conn falls
	// back to one datagram per message.
	OnGSOOff func()
}

// Message is one datagram slot for ReadBatch.
type Message struct {
	Buf  []byte         // receive buffer, used up to its capacity
	N    int            // datagram length, truncated to cap(Buf) (Windows drops a longer datagram)
	Addr netip.AddrPort // source; IPv4-mapped IPv6 addresses are unmapped
}

// Conn reads datagrams from a UDP socket in batches and creates Writers for it.
type Conn struct {
	uc    *net.UDPConn
	mmsg  bool        // recvmmsg/sendmmsg in use
	gso   atomic.Bool // UDP GSO in use; cleared if the kernel refuses a GSO message
	inet6 bool        // AF_INET6 socket: destinations are written as IPv6 socket addresses
	sys   sysConn

	onGSOOff func()
}

// New wraps uc. Batching and GSO are used only where the platform and the kernel
// support them; otherwise every datagram takes its own system call.
func New(uc *net.UDPConn, opt Options) *Conn {
	c := &Conn{uc: uc, onGSOOff: opt.OnGSOOff}
	if opt.Batching {
		c.mmsg = c.initSys()
		c.gso.Store(c.mmsg && opt.GSO && c.gsoSupported())
	}
	return c
}

// Batching reports whether c reads and writes with recvmmsg and sendmmsg.
func (c *Conn) Batching() bool { return c.mmsg }

// GSO reports whether Writers on c currently use UDP GSO.
func (c *Conn) GSO() bool { return c.gso.Load() }

// ReadBatch waits until at least one datagram is queued on the socket, then reads as
// many of the queued datagrams as fit in ms without waiting further. It returns the
// number of slots filled. Only one goroutine may call ReadBatch at a time.
func (c *Conn) ReadBatch(ms []Message) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	if c.mmsg {
		return c.readMmsg(ms)
	}
	m := &ms[0]
	for {
		n, src, err := c.uc.ReadFromUDPAddrPort(m.Buf[:cap(m.Buf)])
		if err != nil {
			if tooLong(err) {
				// Windows: the datagram was longer than the buffer and comes without its
				// source, so it cannot be returned truncated. Skip it rather than return
				// an error, so that a sender of oversized datagrams cannot make the
				// caller treat the socket as failing.
				continue
			}
			return 0, err
		}
		m.N, m.Addr = n, netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		return 1, nil
	}
}

// Dest is a destination address converted once to the socket's own address format,
// so that queuing a datagram does no address conversion.
type Dest struct {
	ap    netip.AddrPort
	sa    [sizeofSockaddr]byte
	salen uint32
}

// Dest prepares ap as a destination for datagrams written through c.
func (c *Conn) Dest(ap netip.AddrPort) Dest {
	d := Dest{ap: ap}
	if c.mmsg {
		c.fillSockaddr(&d)
	}
	return d
}

// AddrPort returns the destination address.
func (d *Dest) AddrPort() netip.AddrPort { return d.ap }

// Result counts the outcome of one Flush.
type Result struct {
	Packets  [MaxTags]int // datagrams sent, by tag
	Bytes    [MaxTags]int
	Failed   int // datagrams the kernel refused, or whose address the socket cannot reach
	Calls    int // system calls made
	Messages int // messages the kernel accepted; with GSO one message carries several datagrams
}

// Writer queues datagrams and sends them on Flush. A Writer belongs to one goroutine
// at a time; any number of Writers may share a Conn.
type Writer struct {
	c    *Conn
	bufs [][]byte
	dsts []*Dest
	tags []uint8
	sys  sysWriter
}

// NewWriter returns an empty Writer for c.
func (c *Conn) NewWriter() *Writer {
	w := &Writer{c: c}
	if c.mmsg {
		w.initSys()
	}
	return w
}

// Add queues b for d; neither may change until Flush returns. The datagram is counted
// under tag, which must be below MaxTags, in the Result of the Flush that sends it.
// With GSO, datagrams added one after another for the same *Dest travel as one
// message when all but the last of them have the same length and the last is no
// longer.
func (w *Writer) Add(b []byte, d *Dest, tag uint8) {
	w.bufs = append(w.bufs, b)
	w.dsts = append(w.dsts, d)
	w.tags = append(w.tags, tag)
}

// Len returns the number of queued datagrams.
func (w *Writer) Len() int { return len(w.bufs) }

// Flush sends the queued datagrams and empties the queue. Datagrams for one
// destination are sent in the order they were added. A datagram the kernel refuses is
// counted in Result.Failed and skipped.
func (w *Writer) Flush() Result {
	var res Result
	if len(w.bufs) == 0 {
		return res
	}
	if w.c.mmsg {
		w.flushMmsg(&res)
	} else {
		for i, b := range w.bufs {
			res.Calls++
			if _, err := w.c.uc.WriteToUDPAddrPort(b, w.dsts[i].ap); err != nil {
				res.Failed++
				continue
			}
			res.Messages++
			res.Packets[w.tags[i]]++
			res.Bytes[w.tags[i]] += len(b)
		}
	}
	// Drop the references to the callers' buffers and destinations.
	clear(w.bufs)
	clear(w.dsts)
	w.bufs, w.dsts, w.tags = w.bufs[:0], w.dsts[:0], w.tags[:0]
	return res
}

// count adds datagram i to res as sent.
func (w *Writer) count(res *Result, i int) {
	t := w.tags[i]
	res.Packets[t]++
	res.Bytes[t] += len(w.bufs[i])
}
