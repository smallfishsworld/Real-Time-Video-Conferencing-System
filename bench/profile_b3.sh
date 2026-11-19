#!/usr/bin/env bash
# CPU profile of the v2 server under the B3 load: the same namespace, pinning and load
# generator as bench/run_b3.sh, at one participant count, with the server's pprof
# endpoint enabled. Writes bench/results/pprof_<label>.pb.gz (the raw profile) and
# bench/results/pprof_<label>.txt (top functions and a breakdown by code path).
#
# Usage: sudo bench/profile_b3.sh                      (128 participants, label "v2")
#        PARTICIPANTS=96 LABEL=v2-batched bench/profile_b3.sh
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

PARTICIPANTS=${PARTICIPANTS:-128}
LABEL=${LABEL:-v2}
SECONDS_PROFILED=${SECONDS_PROFILED:-15}
SERVER_CPU=${SERVER_CPU:-1}
LOADGEN_CPUS=${LOADGEN_CPUS:-2,3}
NS=${NS_PREFIX}3
OUT_DIR="$TMPDIR_BENCH/profile"

if [ "${1:-}" = --locked ]; then
  trap cleanup_netns EXIT INT TERM
  cleanup_netns
  ip netns add "$NS"
  ip -n "$NS" link set lo up
  ip netns exec "$NS" taskset -c "$SERVER_CPU" "$CONFSERVER" -control-addr 127.0.0.1:6100 \
    -media-addr 127.0.0.1:6100 -metrics-addr 127.0.0.1:6101 -pprof -log-level warn \
    >"$OUT_DIR/server.log" 2>&1 &
  pid=$!
  wait_tcp "$NS" 127.0.0.1 6100
  log "profiling the v2 server with $PARTICIPANTS participants for ${SECONDS_PROFILED}s"
  # Window = warm-up (5 s) + profile + 2 s, so the profile sees steady load only.
  ip netns exec "$NS" taskset -c "$LOADGEN_CPUS" "$LOADGEN" -wire v2 -participants "$PARTICIPANTS" \
    -frames "$BENCH/data/frames" -duration "$((SECONDS_PROFILED + 2))s" -server-pid "$pid" \
    -label "profile-$LABEL" -max-mbps 4000 -out "$OUT_DIR/loadgen-$LABEL.json" \
    2>"$OUT_DIR/loadgen-$LABEL.log" &
  lg=$!
  sleep 8 # connection setup and the load generator's 5 s warm-up
  ip netns exec "$NS" curl -fsS --noproxy '*' -o "$RESULTS/pprof_$LABEL.pb.gz" \
    "http://127.0.0.1:6101/debug/pprof/profile?seconds=$SECONDS_PROFILED"
  wait "$lg"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  exit 0
fi

require_root
prepare
mkdir -p "$OUT_DIR"
log "waiting for the bench lock ($LOCK)"
flock "$LOCK" "$0" --locked
"$PY" "$BENCH/analyze_pprof.py" "$RESULTS/pprof_$LABEL.pb.gz" "$CONFSERVER" "$OUT_DIR/loadgen-$LABEL.json" \
  >"$RESULTS/pprof_$LABEL.txt"
cat "$RESULTS/pprof_$LABEL.txt" | head -40
