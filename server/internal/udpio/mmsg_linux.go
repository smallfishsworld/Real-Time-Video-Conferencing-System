//go:build linux && (amd64 || arm64)

package udpio

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const sizeofSockaddr = unix.SizeofSockaddrInet6

// A UDP_SEGMENT control message: a cmsghdr followed by the segment size (uint16).
var (
	cmsgSpace   = unix.CmsgSpace(2)
	cmsgDataOff = unix.CmsgLen(0)
)

// mmsghdr is struct mmsghdr from <sys/socket.h> on 64-bit Linux.
type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
	_   [4]byte
}

// mmsgs holds the message headers of one direction and the arguments and results of
// the system call in flight. The callback handed to syscall.RawConn is built once and
// reads its arguments from here, so a call allocates nothing.
type mmsgs struct {
	rc     syscall.RawConn
	hs     []mmsghdr
	iovs   []unix.Iovec
	off, n int // the call covers hs[off : off+n]
	done   int // messages the call transferred
	errno  syscall.Errno
	fn     func(fd uintptr) bool
}

func newMmsgs(rc syscall.RawConn, trap uintptr) *mmsgs {
	s := &mmsgs{rc: rc}
	s.fn = func(fd uintptr) bool {
		for {
			r, _, e := unix.Syscall6(trap, fd, uintptr(unsafe.Pointer(&s.hs[s.off])), uintptr(s.n), 0, 0, 0)
			switch e {
			case unix.EINTR:
				continue
			case unix.EAGAIN:
				return false // wait until the socket is ready, then try again
			}
			s.done, s.errno = int(r), e
			return true
		}
	}
	return s
}

func (s *mmsgs) grow(n int) {
	if len(s.hs) < n {
		size := max(n, 2*len(s.hs))
		s.hs = make([]mmsghdr, size)
		s.iovs = make([]unix.Iovec, size)
	}
}

type sysConn struct {
	rd     *mmsgs
	names  [][sizeofSockaddr]byte
	zoneID uint32 // last IPv6 scope seen, and its name
	zone   string
}

type sysWriter struct {
	wr   *mmsgs     // one header per message; iovecs one per datagram
	one  *mmsgs     // a single header, for resending a refused GSO message
	msgs []msgRange // the datagrams of each message
	ctrl []byte     // a UDP_SEGMENT control message per header
}

// msgRange is the datagrams w.bufs[first : first+n] of one message.
type msgRange struct{ first, n int }

func (c *Conn) initSys() bool {
	rc, err := c.uc.SyscallConn()
	if err != nil {
		return false
	}
	var sa unix.Sockaddr
	var serr error
	if err := rc.Control(func(fd uintptr) { sa, serr = unix.Getsockname(int(fd)) }); err != nil || serr != nil {
		return false
	}
	switch sa.(type) {
	case *unix.SockaddrInet4:
		c.inet6 = false
	case *unix.SockaddrInet6:
		c.inet6 = true
	default:
		return false
	}
	c.sys.rd = newMmsgs(rc, unix.SYS_RECVMMSG)
	return true
}

func (c *Conn) readMmsg(ms []Message) (int, error) {
	s := &c.sys
	rd := s.rd
	n := min(len(ms), maxVLen)
	rd.grow(n)
	if len(s.names) < n {
		s.names = make([][sizeofSockaddr]byte, len(rd.hs))
	}
	for i := range n {
		b := ms[i].Buf[:cap(ms[i].Buf)]
		rd.iovs[i].Base = unsafe.SliceData(b)
		rd.iovs[i].SetLen(len(b))
		rd.hs[i] = mmsghdr{hdr: unix.Msghdr{Name: &s.names[i][0], Namelen: sizeofSockaddr, Iov: &rd.iovs[i], Iovlen: 1}}
	}
	rd.off, rd.n = 0, n
	if err := rd.rc.Read(rd.fn); err != nil {
		return 0, err
	}
	if rd.errno != 0 {
		return 0, os.NewSyscallError("recvmmsg", rd.errno)
	}
	for i := range rd.done {
		h := &rd.hs[i]
		ms[i].N = int(h.len)
		ms[i].Addr = c.parseSockaddr(s.names[i][:min(int(h.hdr.Namelen), sizeofSockaddr)])
	}
	return rd.done, nil
}

func (c *Conn) parseSockaddr(b []byte) netip.AddrPort {
	if len(b) < 4 {
		return netip.AddrPort{}
	}
	port := binary.BigEndian.Uint16(b[2:4])
	switch binary.NativeEndian.Uint16(b[0:2]) {
	case unix.AF_INET:
		if len(b) < unix.SizeofSockaddrInet4 {
			return netip.AddrPort{}
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port)
	case unix.AF_INET6:
		if len(b) < unix.SizeofSockaddrInet6 {
			return netip.AddrPort{}
		}
		a := netip.AddrFrom16([16]byte(b[8:24]))
		if a.Is4In6() {
			return netip.AddrPortFrom(a.Unmap(), port)
		}
		if id := binary.NativeEndian.Uint32(b[24:28]); id != 0 {
			a = a.WithZone(c.zoneName(id))
		}
		return netip.AddrPortFrom(a, port)
	}
	return netip.AddrPort{}
}

// zoneName names an IPv6 scope the way ReadFromUDPAddrPort does: the interface name,
// or the index as a number when there is no such interface.
func (c *Conn) zoneName(id uint32) string {
	if id != c.sys.zoneID || c.sys.zone == "" {
		name := strconv.FormatUint(uint64(id), 10)
		if ifi, err := net.InterfaceByIndex(int(id)); err == nil {
			name = ifi.Name
		}
		c.sys.zoneID, c.sys.zone = id, name
	}
	return c.sys.zone
}

func scopeID(zone string) uint32 {
	if zone == "" {
		return 0
	}
	if n, err := strconv.ParseUint(zone, 10, 32); err == nil {
		return uint32(n)
	}
	if ifi, err := net.InterfaceByName(zone); err == nil {
		return uint32(ifi.Index)
	}
	return 0
}

// fillSockaddr writes d's address as a sockaddr_in (IPv4 socket) or sockaddr_in6
// (IPv6 socket, IPv4 destinations as IPv4-mapped addresses). An address the socket
// cannot reach keeps salen 0, and the kernel refuses datagrams sent to it.
func (c *Conn) fillSockaddr(d *Dest) {
	a, port := d.ap.Addr(), d.ap.Port()
	if !a.IsValid() {
		return
	}
	if !c.inet6 {
		a = a.Unmap()
		if !a.Is4() {
			return
		}
		binary.NativeEndian.PutUint16(d.sa[0:2], unix.AF_INET)
		binary.BigEndian.PutUint16(d.sa[2:4], port)
		a4 := a.As4()
		copy(d.sa[4:8], a4[:])
		d.salen = unix.SizeofSockaddrInet4
		return
	}
	binary.NativeEndian.PutUint16(d.sa[0:2], unix.AF_INET6)
	binary.BigEndian.PutUint16(d.sa[2:4], port)
	a16 := a.As16()
	copy(d.sa[8:24], a16[:])
	binary.NativeEndian.PutUint32(d.sa[24:28], scopeID(a.Zone()))
	d.salen = unix.SizeofSockaddrInet6
}

func (c *Conn) gsoSupported() bool {
	var serr error
	err := c.sys.rd.rc.Control(func(fd uintptr) {
		_, serr = unix.GetsockoptInt(int(fd), unix.SOL_UDP, unix.UDP_SEGMENT)
	})
	return err == nil && serr == nil
}

func (w *Writer) initSys() {
	rc := w.c.sys.rd.rc
	w.sys.wr = newMmsgs(rc, unix.SYS_SENDMMSG)
	w.sys.one = newMmsgs(rc, unix.SYS_SENDMMSG)
	w.sys.one.grow(1)
}

// growCtrl makes room for n UDP_SEGMENT control messages; only the segment sizes
// change afterwards.
func (w *Writer) growCtrl(n int) {
	if len(w.sys.ctrl) >= n*cmsgSpace {
		return
	}
	w.sys.ctrl = make([]byte, n*cmsgSpace)
	for m := range n {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&w.sys.ctrl[m*cmsgSpace]))
		h.Level, h.Type = unix.SOL_UDP, unix.UDP_SEGMENT
		h.SetLen(unix.CmsgLen(2))
	}
}

func (w *Writer) flushMmsg(res *Result) {
	wr := w.sys.wr
	n := len(w.bufs)
	wr.grow(n)
	if len(w.sys.msgs) < len(wr.hs) {
		w.sys.msgs = make([]msgRange, len(wr.hs))
	}
	gso := w.c.gso.Load()
	if gso {
		w.growCtrl(len(wr.hs))
	}
	for i, b := range w.bufs {
		wr.iovs[i].Base = unsafe.SliceData(b)
		wr.iovs[i].SetLen(len(b))
	}
	// Build the messages: one per datagram, or with GSO one per run of datagrams for
	// the same destination whose lengths all equal the first's except a shorter last.
	m := 0
	for i := 0; i < n; {
		d := w.dsts[i]
		size, j := len(w.bufs[i]), i+1
		if gso {
			for total := size; j < n && j-i < maxSegments && w.dsts[j] == d; {
				l := len(w.bufs[j])
				if l > size || total+l > maxGSOBytes {
					break
				}
				total += l
				j++
				if l < size {
					break
				}
			}
		}
		h := &wr.hs[m]
		*h = mmsghdr{hdr: unix.Msghdr{Name: &d.sa[0], Namelen: d.salen, Iov: &wr.iovs[i]}}
		h.hdr.SetIovlen(j - i)
		if j-i > 1 {
			c := w.sys.ctrl[m*cmsgSpace : (m+1)*cmsgSpace]
			binary.NativeEndian.PutUint16(c[cmsgDataOff:], uint16(size))
			h.hdr.Control = &c[0]
			h.hdr.SetControllen(cmsgSpace)
		}
		w.sys.msgs[m] = msgRange{first: i, n: j - i}
		m++
		i = j
	}
	for off := 0; off < m; {
		wr.off, wr.n = off, min(m-off, maxVLen)
		res.Calls++
		if err := wr.rc.Write(wr.fn); err != nil { // socket closed
			for _, mr := range w.sys.msgs[off:m] {
				res.Failed += mr.n
			}
			return
		}
		if wr.errno != 0 || wr.done <= 0 {
			// The kernel refused the first message of the call; the ones before it
			// in the batch went out with the previous call.
			if mr := w.sys.msgs[off]; mr.n > 1 {
				w.resend(res, mr)
			} else {
				res.Failed++
			}
			off++
			continue
		}
		res.Messages += wr.done
		for _, mr := range w.sys.msgs[off : off+wr.done] {
			for i := mr.first; i < mr.first+mr.n; i++ {
				w.count(res, i)
			}
		}
		off += wr.done
	}
}

// resend sends the datagrams of a refused GSO message one at a time. If any of them
// goes through, the kernel refused GSO itself (for example on a device without
// checksum offload, or before Linux 4.18), and the Conn stops using it.
func (w *Writer) resend(res *Result, mr msgRange) {
	one := w.sys.one
	for i := mr.first; i < mr.first+mr.n; i++ {
		d := w.dsts[i]
		one.hs[0] = mmsghdr{hdr: unix.Msghdr{Name: &d.sa[0], Namelen: d.salen, Iov: &w.sys.wr.iovs[i], Iovlen: 1}}
		one.off, one.n = 0, 1
		res.Calls++
		if err := one.rc.Write(one.fn); err != nil || one.errno != 0 || one.done != 1 {
			res.Failed++
			continue
		}
		res.Messages++
		w.count(res, i)
		if w.c.gso.Swap(false) && w.c.onGSOOff != nil {
			w.c.onGSOOff()
		}
	}
}
