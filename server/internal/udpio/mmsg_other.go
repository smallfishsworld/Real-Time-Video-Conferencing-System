//go:build !(linux && (amd64 || arm64))

package udpio

const sizeofSockaddr = 28

type sysConn struct{}

type sysWriter struct{}

// Without recvmmsg and sendmmsg, New never enables batching, so the batched paths
// below are never reached.

func (c *Conn) initSys() bool { return false }

func (c *Conn) gsoSupported() bool { return false }

func (c *Conn) readMmsg([]Message) (int, error) { panic("udpio: batching unavailable") }

func (c *Conn) fillSockaddr(*Dest) {}

func (w *Writer) initSys() {}

func (w *Writer) flushMmsg(*Result) { panic("udpio: batching unavailable") }
