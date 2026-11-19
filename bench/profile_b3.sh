#!/usr/bin/env bash
# CPU profiles of the v2 server under the B3 load: the same namespace setup (lo
# gso_max_segs 1, see bench/run_b3.sh), pinning and load generator as B3, at one
# participant count, with the server's pprof endpoint enabled. One invocation profiles
# every server variant in PROFILES (name=flags, flags joined by commas) in one hold of
# the bench lock.
#
# For each variant NAME it writes bench/results/pprof_NAME.pb.gz (the raw profile),
# pprof_NAME.txt (the load, the profile's header, a breakdown by code path, the
# server's system-call counters and the top functions) and pprof_NAME.json (the
# provenance: run ID, command, server flags, build record with binary hashes, the load
# generator's result and the server counters). Raw files stay in
# bench/tmp/profile/<run ID>/.
#
# Usage: sudo bench/profile_b3.sh
#        PARTICIPANTS=96 PROFILES="v2-gso=" bench/profile_b3.sh
# Environment: PARTICIPANTS (128), SECONDS_PROFILED (15), GSO_MAX_SEGS (1),
#   PROFILES ("v2-unbatched=-udp-batch=1 v2-sendmmsg=-udp-gso=false v2-gso="),
#   SERVER_CPU (1), LOADGEN_CPUS (2,3), MAX_MBPS (12000), MAX_PPS (1500000).
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

PARTICIPANTS=${PARTICIPANTS:-128}
SECONDS_PROFILED=${SECONDS_PROFILED:-15}
GSO_MAX_SEGS=${GSO_MAX_SEGS:-1}
PROFILES=${PROFILES:-"v2-unbatched=-udp-batch=1 v2-sendmmsg=-udp-gso=false v2-gso="}
SERVER_CPU=${SERVER_CPU:-1}
LOADGEN_CPUS=${LOADGEN_CPUS:-2,3}
MAX_MBPS=${MAX_MBPS:-12000}
MAX_PPS=${MAX_PPS:-1500000}
NS="${NS_PREFIX}prof"
RUNDIR="$TMPDIR_BENCH/profile/$CS305_RUN_ID"
COMMAND="PARTICIPANTS=$PARTICIPANTS SECONDS_PROFILED=$SECONDS_PROFILED GSO_MAX_SEGS=$GSO_MAX_SEGS PROFILES=\"$PROFILES\" SERVER_CPU=$SERVER_CPU LOADGEN_CPUS=$LOADGEN_CPUS MAX_MBPS=$MAX_MBPS MAX_PPS=$MAX_PPS bench/profile_b3.sh"

setup_ns() {
  cleanup_netns
  ip netns add "$NS"
  ip -n "$NS" link set lo up
  if [ "$GSO_MAX_SEGS" != 0 ]; then
    ip -n "$NS" link set lo gso_max_segs "$GSO_MAX_SEGS"
  fi
}

# profile NAME FLAGS: one profile of the server started with FLAGS (comma-separated).
profile() {
  local name=$1 args pid lg
  args=$(printf '%s' "$2" | tr ',' ' ')
  setup_ns
  # shellcheck disable=SC2086 # args is a list of flags
  ip netns exec "$NS" taskset -c "$SERVER_CPU" "$CONFSERVER" -control-addr 127.0.0.1:6100 \
    -media-addr 127.0.0.1:6100 -metrics-addr 127.0.0.1:6101 -pprof -log-level warn $args \
    >"$RUNDIR/$name.server.log" 2>&1 &
  pid=$!
  wait_tcp "$NS" 127.0.0.1 6100
  log "profiling $name ($args) with $PARTICIPANTS participants for ${SECONDS_PROFILED}s"
  # Window = warm-up (5 s) + profile + 2 s, so the profile sees steady load only.
  ip netns exec "$NS" taskset -c "$LOADGEN_CPUS" "$LOADGEN" -wire v2 -participants "$PARTICIPANTS" \
    -frames "$BENCH/data/frames" -duration "$((SECONDS_PROFILED + 2))s" -server-pid "$pid" \
    -server-core "$SERVER_CPU" -label "$CS305_RUN_ID/$name" -max-mbps "$MAX_MBPS" -max-pps "$MAX_PPS" \
    -out "$RUNDIR/$name.loadgen.json" 2>"$RUNDIR/$name.loadgen.log" &
  lg=$!
  sleep 8 # connection setup and the load generator's 5 s warm-up
  ip netns exec "$NS" curl -fsS --noproxy '*' -o "$RUNDIR/$name.pb.gz" \
    "http://127.0.0.1:6101/debug/pprof/profile?seconds=$SECONDS_PROFILED"
  wait "$lg"
  scrape_metrics "$NS" >"$RUNDIR/$name.metrics"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  cleanup_netns
}

if [ "${1:-}" = --locked ]; then
  trap cleanup_netns EXIT
  trap 'exit 130' INT TERM
  for p in $PROFILES; do profile "${p%%=*}" "${p#*=}"; done
  exit 0
fi

require_root
trap cleanup_netns EXIT # only this run's namespaces
trap 'exit 130' INT TERM
prepare
mkdir -p "$RUNDIR"
log "waiting for the bench lock ($LOCK)"
locked "$0" --locked
for p in $PROFILES; do
  name=${p%%=*}
  cp "$RUNDIR/$name.pb.gz" "$RESULTS/pprof_$name.pb.gz"
  cp "$RUNDIR/$name.metrics" "$RESULTS/pprof_$name.metrics"
  "$PY" "$BENCH/analyze_pprof.py" "$RUNDIR/$name.pb.gz" "$CONFSERVER" "$RUNDIR/$name.loadgen.json" \
    "$RUNDIR/$name.metrics" --provenance "$RESULTS/pprof_$name.json" --build "$BUILD_DIR/build.json" \
    --run-id "$CS305_RUN_ID" --command "sudo $COMMAND" --server-args "$(printf '%s' "${p#*=}" | tr ',' ' ')" \
    --raw-dir "$RUNDIR" >"$RESULTS/pprof_$name.txt"
  head -20 "$RESULTS/pprof_$name.txt"
done
