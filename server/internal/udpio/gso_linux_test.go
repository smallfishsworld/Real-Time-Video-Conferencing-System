//go:build linux && (amd64 || arm64)

package udpio

import (
	"bytes"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// If the kernel refuses a GSO message, the Writer resends its datagrams one at a time
// and stops using GSO. A control message of the wrong length makes the kernel refuse.
func TestWriterGSOFallback(t *testing.T) {
	c := New(listen(t, "udp4", "127.0.0.1:0"), Options{Batching: true, GSO: true})
	if !c.GSO() {
		t.Skip("kernel without UDP GSO")
	}
	rx := listen(t, "udp4", "127.0.0.1:0")
	d := c.Dest(addrOf(rx))
	w := c.NewWriter()
	w.growCtrl(8)
	for m := range 8 {
		(*unix.Cmsghdr)(unsafe.Pointer(&w.sys.ctrl[m*cmsgSpace])).SetLen(unix.CmsgLen(4))
	}
	sent := make([][]byte, 5)
	for i := range sent {
		sent[i] = bytes.Repeat([]byte{byte(i)}, 1000)
		w.Add(sent[i], &d, 0)
	}
	res := w.Flush()
	if res.Failed != 0 || res.Packets[0] != 5 || res.Messages != 5 || c.GSO() {
		t.Fatalf("Result = %+v, GSO on: %v; want 5 single messages and GSO off", res, c.GSO())
	}
	buf := make([]byte, 2048)
	for i := range sent {
		_ = rx.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := rx.ReadFromUDPAddrPort(buf)
		if err != nil || !bytes.Equal(buf[:n], sent[i]) {
			t.Fatalf("datagram %d: %d bytes, %v", i, n, err)
		}
	}
}
