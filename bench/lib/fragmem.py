"""Print the namespace's IPv4 reassembly queue count and memory twice a second.

Usage: fragmem.py END_AT   (CLOCK_MONOTONIC seconds). Output lines: "<monotonic> <inuse> <bytes>".
"""

import sys
import time

end = float(sys.argv[1])
while time.monotonic() < end:
    with open("/proc/net/sockstat") as f:
        for line in f:
            if line.startswith("FRAG:"):
                fields = line.split()
                print(f"{time.monotonic():.3f} {fields[2]} {fields[4]}", flush=True)
    time.sleep(0.5)
