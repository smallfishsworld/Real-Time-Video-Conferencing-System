package media

import (
	"math/bits"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// rtxSlot is the size of one stored packet: the largest datagram any sender in this
// system produces. Larger packets are forwarded but not stored.
const rtxSlot = protocol.MaxSendDatagram

// rtxRing keeps the most recent video packets (fragments and parity packets) of one
// sender, indexed by RTP sequence number, so that the server can answer a receiver's
// NACK. Only the Serve goroutine touches it; the memory is allocated with the first
// packet stored.
type rtxRing struct {
	size int // a power of two, so the index stays continuous when sequence numbers wrap
	data []byte
	lens []uint16 // 0 marks an empty slot
	seqs []uint16
	top  uint16 // newest sequence number stored
}

func newRTXRing(packets int) *rtxRing {
	if packets <= 0 {
		return nil
	}
	size := 1 << bits.Len(uint(packets-1))
	return &rtxRing{size: min(size, 1<<15)}
}

func (q *rtxRing) store(pkt []byte, seq uint16) {
	if len(pkt) > rtxSlot {
		return
	}
	if q.data == nil {
		q.data = make([]byte, q.size*rtxSlot)
		q.lens = make([]uint16, q.size)
		q.seqs = make([]uint16, q.size)
		q.top = seq
	}
	i := int(seq) & (q.size - 1)
	copy(q.data[i*rtxSlot:], pkt)
	q.lens[i], q.seqs[i] = uint16(len(pkt)), seq
	if protocol.SeqDiff(seq, q.top) > 0 {
		q.top = seq
	}
}

// get returns the stored packet with sequence number seq, or nil. It refuses packets
// within guard of being overwritten: the returned slice is sent at the end of the
// current read, and up to guard packets of this sender may be stored before then.
func (q *rtxRing) get(seq uint16, guard int) []byte {
	if q.data == nil {
		return nil
	}
	if age := protocol.SeqDiff(q.top, seq); age < 0 || age >= q.size-guard {
		return nil
	}
	i := int(seq) & (q.size - 1)
	if q.lens[i] == 0 || q.seqs[i] != seq {
		return nil
	}
	return q.data[i*rtxSlot : i*rtxSlot+int(q.lens[i])]
}
