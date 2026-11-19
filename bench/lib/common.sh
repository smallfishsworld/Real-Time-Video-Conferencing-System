# Shared helpers for the benchmark scripts (sourced, not executed).
#
# Every benchmark runs inside network namespaces whose names start with "cs305b"; they
# and their nftables rules disappear with the namespace. cleanup_netns removes all of
# them and is installed as an EXIT trap by each script.

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BENCH="$ROOT/bench"
TMPDIR_BENCH="$BENCH/tmp"
RESULTS="$BENCH/results"
LOCK=${BENCH_LOCK:-/home/user/work/bench.lock}
PY="$ROOT/client/.venv/bin/python"
CONFSERVER="$TMPDIR_BENCH/confserver"
LOADGEN="$TMPDIR_BENCH/loadgen"
NS_PREFIX=cs305b

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    echo "this benchmark needs root (network namespaces and nftables)" >&2
    exit 1
  fi
}

mono() { "$PY" -c 'import time; print(f"{time.monotonic():.3f}")'; }
mono_plus() { "$PY" -c "import time; print(f'{time.monotonic() + $1:.3f}')"; }

prepare() {
  mkdir -p "$TMPDIR_BENCH" "$RESULTS"
  if [ ! -x "$PY" ]; then
    (cd "$ROOT/client" && uv sync --frozen --quiet)
  fi
  log "building confserver and loadgen"
  (cd "$ROOT/server" && nice -n 19 go build -o "$CONFSERVER" ./cmd/confserver)
  if [ -f "$ROOT/server/cmd/loadgen/main.go" ]; then
    (cd "$ROOT/server" && nice -n 19 go build -o "$LOADGEN" ./cmd/loadgen)
  fi
  if [ ! -f "$BENCH/data/frames/frame_00999.jpg" ]; then
    log "generating the frame corpus"
    nice -n 19 "$PY" "$BENCH/gen_frames.py" --count 1000
  fi
}

cleanup_netns() {
  local ns
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep "^${NS_PREFIX}" || true); do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
    ip netns del "$ns" 2>/dev/null || true
  done
}

# wait_tcp NS HOST PORT: wait until something listens on HOST:PORT inside NS.
wait_tcp() {
  local ns=$1 host=$2 port=$3 i
  for i in $(seq 1 100); do
    if ip netns exec "$ns" "$PY" -c "import socket,sys; s=socket.socket(); s.settimeout(0.2); sys.exit(s.connect_ex(('$host',$port)))" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  echo "nothing listening on $host:$port in $ns" >&2
  return 1
}

# snmp NS: IP reassembly and UDP counters of the namespace as JSON.
snmp() {
  ip netns exec "$1" "$PY" - <<'EOF'
import json
lines = open("/proc/net/snmp").read().splitlines()
out = {}
for head, vals in zip(lines[::2], lines[1::2]):
    proto, names = head.split(":", 1)
    if proto in ("Ip", "Udp"):
        out[proto] = dict(zip(names.split(), map(int, vals.split(":", 1)[1].split())))
print(json.dumps(out))
EOF
}
