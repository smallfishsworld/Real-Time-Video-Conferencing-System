# Shared helpers for the benchmark scripts (sourced, not executed).
#
# One invocation of a benchmark script is one run: it gets a run ID (UTC time and
# process ID, CS305_RUN_ID), builds its binaries into bench/tmp/build/<run ID>/ with a
# build record, writes its raw files into a directory of its own, and its analysis
# reads only that directory. Its network namespaces are called
# cs305b-<invocation>-<name> and cleanup_netns removes only those, so two invocations
# (or an invocation waiting for the lock) never touch each other's namespaces. The
# namespaces' nftables rules disappear with them. Scripts re-execute themselves for
# the steps that hold the lock; CS305_RUN_ID and CS305_INVOCATION carry over.

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BENCH="$ROOT/bench"
TMPDIR_BENCH="$BENCH/tmp"
RESULTS="$BENCH/results"
# Timing runs are serialised with flock on this file. Point BENCH_LOCK at a lock that
# other users of the machine take as well.
LOCK=${BENCH_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/cs305-bench.lock}
PY="$ROOT/client/.venv/bin/python"
CS305_INVOCATION=${CS305_INVOCATION:-$$}
CS305_RUN_ID=${CS305_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$CS305_INVOCATION}
export CS305_INVOCATION CS305_RUN_ID
NS_PREFIX="cs305b-$CS305_INVOCATION-"
BUILD_DIR="$TMPDIR_BENCH/build/$CS305_RUN_ID"
CONFSERVER="$BUILD_DIR/confserver"
LOADGEN="$BUILD_DIR/loadgen"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    echo "this benchmark needs root (network namespaces and nftables)" >&2
    exit 1
  fi
}

mono() { "$PY" -c 'import time; print(f"{time.monotonic():.3f}")'; }
mono_plus() { "$PY" -c "import time; print(f'{time.monotonic() + $1:.3f}')"; }

# prepare: the Python environment, this run's binaries and build record, and the
# frame corpus. Runs outside the lock; nothing another run uses is touched.
prepare() {
  mkdir -p "$TMPDIR_BENCH" "$RESULTS" "$BUILD_DIR"
  if [ ! -x "$PY" ]; then
    (cd "$ROOT/client" && uv sync --frozen --quiet)
  fi
  log "building confserver and loadgen into $BUILD_DIR"
  # Two parallel compile jobs at the lowest priority: the machine may be shared.
  (cd "$ROOT/server" && export GOFLAGS="${GOFLAGS:--p=2}" &&
    nice -n 19 go build -o "$CONFSERVER" ./cmd/confserver &&
    nice -n 19 go build -o "$LOADGEN" ./cmd/loadgen)
  chmod a-w "$CONFSERVER" "$LOADGEN"
  write_build_record "$BUILD_DIR/build.json" "$CONFSERVER" "$LOADGEN"
  if [ ! -f "$BENCH/data/frames/frame_00999.jpg" ]; then
    log "generating the frame corpus"
    nice -n 19 "$PY" "$BENCH/gen_frames.py" --count 1000
  fi
}

# write_build_record OUT [BINARY...]: what the binaries were built from (commit, the
# uncommitted paths outside bench/results and the SHA-256 of their diff against the
# commit, null when there is none), when, the Go version, and each binary's SHA-256.
# The analysis scripts copy it into the result files.
write_build_record() {
  local out=$1 first=1 bin diff
  shift
  diff=$(git -C "$ROOT" diff HEAD -- server client bench legacy ':!bench/results')
  {
    printf '{"git_commit": "%s", "utc": "%s", "go": "%s", "git_dirty_files": [' \
      "$(git -C "$ROOT" rev-parse --short HEAD)" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(go version 2>/dev/null)"
    git -C "$ROOT" status --porcelain -- server client bench legacy ':!bench/results' |
      sed 's/\\/\\\\/g; s/"/\\"/g; s/.*/"&"/' | paste -sd, -
    if [ -n "$diff" ]; then
      printf '], "git_diff_sha256": "%s", "binaries": {' "$(printf '%s\n' "$diff" | sha256sum | cut -c1-64)"
    else
      printf '], "git_diff_sha256": null, "binaries": {'
    fi
    for bin in "$@"; do
      [ $first -eq 1 ] || printf ', '
      first=0
      printf '"%s": "%s"' "$(basename "$bin")" "$(sha256sum "$bin" | cut -c1-64)"
    done
    printf '}}\n'
  } >"$out"
}

# cleanup_netns: kill everything in this invocation's namespaces and delete them.
cleanup_netns() {
  local ns
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -F -- "$NS_PREFIX" || true); do
    case $ns in "$NS_PREFIX"*) ;; *) continue ;; esac
    ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
    ip netns del "$ns" 2>/dev/null || true
  done
}

# locked CMD...: run CMD under the bench lock and return its status; 130 when it was
# interrupted, so that callers can stop.
locked() {
  local status=0
  flock "$LOCK" "$@" || status=$?
  return "$status"
}

# wait_all PID...: wait for each process and fail if any of them failed (`wait a b`
# returns only the last one's status).
wait_all() {
  local pid status=0
  for pid in "$@"; do
    wait "$pid" || status=$?
  done
  return "$status"
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

# scrape_metrics NS: the v2 server's batching and GSO state, system-call and packet
# counters and drops, from its /metrics on 127.0.0.1:6101 inside NS.
scrape_metrics() {
  ip netns exec "$1" curl -fsS --noproxy '*' --max-time 5 http://127.0.0.1:6101/metrics |
    grep -E '^confserver_(udp_batching_enabled|udp_gso_enabled|media_read_syscalls_total|video_send_(syscalls|messages)_total|media_packets_(sent|received)_total|media_dropped_packets_total|mixer_overrun_ticks_total)' ||
    true
}
