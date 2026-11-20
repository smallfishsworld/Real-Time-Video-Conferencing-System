package main

import (
	"runtime"

	"golang.org/x/net/ipv4"
)

// readBatch and writeBatch move up to len(ms) datagrams, one buffer each, through pc:
// recvmmsg and sendmmsg on Linux, recvmsg and sendmsg on the other Unix systems. On
// Windows golang.org/x/net implements neither, so there they fall back to readOne and
// writeEach.
func readBatch(pc *ipv4.PacketConn, ms []ipv4.Message) (int, error) {
	if runtime.GOOS == "windows" {
		return readOne(pc, ms)
	}
	return pc.ReadBatch(ms, 0)
}

func writeBatch(pc *ipv4.PacketConn, ms []ipv4.Message) (int, error) {
	if runtime.GOOS == "windows" {
		return writeEach(pc, ms)
	}
	return pc.WriteBatch(ms, 0)
}

// readOne reads one datagram into ms[0] with a single ReadFrom call.
func readOne(pc *ipv4.PacketConn, ms []ipv4.Message) (int, error) {
	n, _, src, err := pc.ReadFrom(ms[0].Buffers[0])
	if err != nil {
		return 0, err
	}
	ms[0].N, ms[0].Addr = n, src
	return 1, nil
}

// writeEach sends the messages with one WriteTo call each and stops at the first error.
func writeEach(pc *ipv4.PacketConn, ms []ipv4.Message) (int, error) {
	for i := range ms {
		if _, err := pc.WriteTo(ms[i].Buffers[0], nil, ms[i].Addr); err != nil {
			return i, err
		}
	}
	return len(ms), nil
}
