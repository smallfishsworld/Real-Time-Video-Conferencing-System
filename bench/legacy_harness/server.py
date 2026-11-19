"""Run the original MainServer (legacy/my_server.py) unmodified on a chosen address.

Usage: python bench/legacy_harness/server.py --ip 127.0.0.1
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import stubs


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--ip", default="127.0.0.1")
    args = ap.parse_args()
    stubs.install(args.ip)
    import my_server  # type: ignore[import-not-found]

    print(f"legacy MainServer on {my_server.SERVER_IP}:{my_server.MAIN_SERVER_PORT}", flush=True)
    my_server.MainServer(my_server.SERVER_IP, my_server.MAIN_SERVER_PORT).run()


if __name__ == "__main__":
    main()
