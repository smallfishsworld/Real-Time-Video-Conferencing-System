"""Check how a UDP GSO message crosses this namespace's loopback.

Sends one 4 x 1200-byte UDP_SEGMENT message to a socket with UDP_GRO set and prints, as
JSON, the size of every read. One read of 4800 bytes means the message went up the
receive path whole (lo's default); four reads mean the kernel split it before lo, as
it does in front of a NIC without UDP segmentation offload (lo gso_max_segs 1).
Linux only; run inside the benchmark's namespace.
"""

from __future__ import annotations

import json
import socket
import struct

SOL_UDP, UDP_SEGMENT, UDP_GRO = 17, 103, 104
SEGMENT, COUNT = 1200, 4


def main() -> None:
    rx = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    rx.bind(("127.0.0.1", 0))
    rx.settimeout(0.3)
    rx.setsockopt(SOL_UDP, UDP_GRO, 1)
    tx = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    gso = [(SOL_UDP, UDP_SEGMENT, struct.pack("H", SEGMENT))]
    tx.sendmsg([bytes(SEGMENT * COUNT)], gso, 0, rx.getsockname())
    reads = []
    try:
        while True:
            data, _anc, _flags, _addr = rx.recvmsg(65536, socket.CMSG_SPACE(4))
            reads.append(len(data))
    except TimeoutError:
        pass
    split = len(reads) == COUNT
    print(json.dumps({"sent_bytes": SEGMENT * COUNT, "segment": SEGMENT, "reads": reads, "split": split}))


if __name__ == "__main__":
    main()
