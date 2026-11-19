"""A userspace UDP relay that delays and drops datagrams in both directions (B1's
harder condition; this kernel has no netem).

    client --> LISTEN --(drop p, delay d)--> upstream socket --> UPSTREAM (the server)
    client <-- LISTEN <--(drop p, delay d)-- upstream socket <-- UPSTREAM

Each client address that sends to LISTEN gets its own upstream socket, as behind a
NAT, so the server sees the relay's socket as the client's media address. Every
datagram is dropped independently with probability p (a seeded random generator, one
draw per datagram and direction) and otherwise sent on after a fixed one-way delay d,
so ordering is kept and the round trip grows by 2d. The relay works on whole UDP
datagrams, after IP reassembly, so it suits v2 (datagrams of at most 1232 bytes), not
the original's 31 kB datagrams. At --end-at (time.monotonic()) plus --linger seconds
it writes its counters as JSON to --stats and exits. Its sockets have 4 MiB receive
buffers; overflows would show in the namespace's UDP RcvbufErrors.

Usage: udp_impair.py --listen 10.77.2.1:7000 --upstream 10.77.0.1:6100 --delay-ms 25
           --loss 0.05 --seed 1 --end-at T --stats out.json
"""

from __future__ import annotations

import argparse
import asyncio
import json
import random
import socket
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

BUFFER = 4 << 20


@dataclass
class Direction:
    received: int = 0
    dropped: int = 0
    sent: int = 0
    send_errors: int = 0
    bytes_received: int = 0


@dataclass
class Counters:
    upstream: Direction = field(default_factory=Direction)  # client -> server
    downstream: Direction = field(default_factory=Direction)  # server -> client
    clients: int = 0


def parse_addr(text: str) -> tuple[str, int]:
    host, _, port = text.rpartition(":")
    return host, int(port)


def udp_socket() -> socket.socket:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    for force, plain in ((33, socket.SO_RCVBUF), (32, socket.SO_SNDBUF)):  # SO_{RCV,SND}BUFFORCE
        try:
            s.setsockopt(socket.SOL_SOCKET, force, BUFFER)  # root: past net.core.[rw]mem_max
        except OSError:
            s.setsockopt(socket.SOL_SOCKET, plain, BUFFER)
    s.setblocking(False)
    return s


class Relay:
    def __init__(self, listen: tuple[str, int], upstream: tuple[str, int], delay: float, loss: float, seed: int):
        self.loop = asyncio.get_running_loop()
        self.upstream = upstream
        self.delay = delay
        self.loss = loss
        self.rng = random.Random(seed)
        self.counters = Counters()
        self.flows: dict[tuple[str, int], socket.socket] = {}
        self.listener = udp_socket()
        self.listener.bind(listen)
        self.loop.add_reader(self.listener, self._from_clients)

    def _pass(self, d: Direction, data: bytes) -> bool:
        d.received += 1
        d.bytes_received += len(data)
        if self.rng.random() < self.loss:
            d.dropped += 1
            return False
        d.sent += 1
        return True

    def _send(self, d: Direction, sock: socket.socket, data: bytes, addr: tuple[str, int] | None) -> None:
        try:
            if addr is None:
                sock.send(data)
            else:
                sock.sendto(data, addr)
        except OSError:
            d.send_errors += 1

    def _from_clients(self) -> None:
        while True:
            try:
                data, client = self.listener.recvfrom(65536)
            except BlockingIOError:
                return
            except OSError:
                continue
            flow = self.flows.get(client)
            if flow is None:
                flow = udp_socket()
                flow.connect(self.upstream)
                self.flows[client] = flow
                self.counters.clients += 1
                self.loop.add_reader(flow, self._from_server, flow, client)
            d = self.counters.upstream
            if self._pass(d, data):
                self.loop.call_later(self.delay, self._send, d, flow, data, None)

    def _from_server(self, flow: socket.socket, client: tuple[str, int]) -> None:
        while True:
            try:
                data = flow.recv(65536)
            except BlockingIOError:
                return
            except OSError:  # an ICMP error for an earlier send
                continue
            d = self.counters.downstream
            if self._pass(d, data):
                self.loop.call_later(self.delay, self._send, d, self.listener, data, client)

    def close(self) -> None:
        for s in (self.listener, *self.flows.values()):
            self.loop.remove_reader(s)
            s.close()


async def run(args: argparse.Namespace) -> dict[str, Any]:
    relay = Relay(parse_addr(args.listen), parse_addr(args.upstream), args.delay_ms / 1000, args.loss, args.seed)
    await asyncio.sleep(max(0.0, args.end_at + args.linger - time.monotonic()))
    relay.close()
    return {
        "method": "userspace UDP relay (bench/lib/udp_impair.py): independent drops per datagram and "
        "direction, fixed one-way delay, order kept",
        "listen": args.listen,
        "upstream": args.upstream,
        "one_way_delay_ms": args.delay_ms,
        "loss_probability": args.loss,
        "seed": args.seed,
        **asdict(relay.counters),
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", required=True)
    ap.add_argument("--upstream", required=True)
    ap.add_argument("--delay-ms", type=float, default=25.0)
    ap.add_argument("--loss", type=float, default=0.0)
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--end-at", type=float, required=True)
    ap.add_argument("--linger", type=float, default=2.0)
    ap.add_argument("--stats", type=Path, required=True)
    args = ap.parse_args()
    if not 0 <= args.loss <= 1 or not args.delay_ms >= 0:
        ap.error("--loss must be in [0, 1] and --delay-ms >= 0")
    args.stats.write_text(json.dumps(asyncio.run(run(args))) + "\n")


if __name__ == "__main__":
    main()
