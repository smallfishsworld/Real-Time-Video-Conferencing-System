#!/usr/bin/env bash
# B3 relay capacity: how many participants (rooms of 4) each server sustains with
# >= 99 % frame delivery and p99 frame latency < 100 ms, and its CPU time per
# participant-minute at a fixed load.
#
# One network namespace, loopback only. The server is pinned to one CPU and the load
# generator (server/cmd/loadgen) to two others, so the server's capacity is that of
# one core. Both systems get the same offered load: corpus frames (480x480 JPEG q85,
# ~31 kB) at 15 fps per participant plus 16 kHz audio, each in its own wire format
# (v2: RTP fragments + FEC; legacy: one pickled datagram per frame or audio chunk, sent
# from a distinct 127.1.x.y address per participant as the legacy relay requires).
#
# Usage: sudo bench/run_b3.sh
#        WIRES=v2 SIZES="8 16" MTU=1500 bench/run_b3.sh
# Writes bench/results/b3_capacity[_mtu1500].json.
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

WIRES=${WIRES:-"v2 legacy"}
SIZES=${SIZES:-"8 16 32 48 64 96 128 160 192 224 256 320 384 448"}
FIXED=${FIXED:-16}             # participants for the CPU-per-participant-minute run
MTU=${MTU:-65536}              # loopback MTU (65536 is the Linux default; 1500 = Ethernet)
WINDOW=${WINDOW:-20s}
FIXED_WINDOW=${FIXED_WINDOW:-60s}
SERVER_CPU=${SERVER_CPU:-1}
LOADGEN_CPUS=${LOADGEN_CPUS:-2,3}
SUFFIX=""
[ "$MTU" != 65536 ] && SUFFIX="_mtu$MTU"
OUT_DIR="$TMPDIR_BENCH/b3$SUFFIX"
NS=${NS_PREFIX}3

setup_ns() {
  cleanup_netns
  ip netns add "$NS"
  ip -n "$NS" link set lo mtu "$MTU" up
}

# start_server WIRE LOG: starts the server pinned to SERVER_CPU, prints its PID.
start_server() {
  local wire=$1 logf=$2
  if [ "$wire" = v2 ]; then
    ip netns exec "$NS" taskset -c "$SERVER_CPU" "$CONFSERVER" -control-addr 127.0.0.1:6100 \
      -media-addr 127.0.0.1:6100 -metrics-addr "" -log-level warn >"$logf" 2>&1 &
    echo $!
  else
    ip netns exec "$NS" taskset -c "$SERVER_CPU" "$PY" "$BENCH/legacy_harness/server.py" --ip 127.0.0.1 \
      >"$logf" 2>&1 &
    echo $!
  fi
}

# run_point WIRE N WINDOW LABEL: one load-generator run against a fresh server. Returns
# 0 if the size passes, 3 if it fails the criteria, 2 if the run itself failed twice.
run_point() {
  local wire=$1 n=$2 window=$3 label=$4 out pid port=6100 attempt
  out="$OUT_DIR/$wire-$n-$label.json"
  rm -f "$out"
  [ "$wire" = legacy ] && port=8888
  for attempt in 1 2; do
    setup_ns
    pid=$(start_server "$wire" "$OUT_DIR/$wire-$n-$label.server.log")
    wait_tcp "$NS" 127.0.0.1 "$port"
    # ip netns exec runs the command directly, so $pid is the server process itself.
    log "B3 $wire participants=$n ($label, window $window, lo mtu $MTU)"
    if ! ip netns exec "$NS" taskset -c "$LOADGEN_CPUS" "$LOADGEN" -wire "$wire" -participants "$n" \
      -frames "$BENCH/data/frames" -duration "$window" -server-pid "$pid" -label "$label" \
      -max-mbps 4000 -out "$out" 2>"$OUT_DIR/$wire-$n-$label.loadgen.log"; then
      log "loadgen failed for $wire n=$n, attempt $attempt (see $OUT_DIR/$wire-$n-$label.loadgen.log)"
    fi
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    cleanup_netns
    [ -f "$out" ] && break
  done
  [ -f "$out" ] || return 2
  "$PY" "$BENCH/analyze_b3.py" --check "$out"
}

sweep() {
  local wire=$1 n fails=0 status
  for n in $SIZES; do
    set +e
    run_point "$wire" "$n" "$WINDOW" sweep
    status=$?
    set -e
    if [ "$status" -eq 2 ]; then
      log "$wire: the run at n=$n failed twice; stopping the sweep"
      break
    fi
    if [ "$status" -eq 0 ]; then fails=0; else fails=$((fails + 1)); fi
    if [ "$fails" -ge 2 ]; then
      log "$wire failed twice in a row; stopping the sweep"
      break
    fi
  done
  set +e
  run_point "$wire" "$FIXED" "$FIXED_WINDOW" fixed
  set -e
}

if [ "${1:-}" = --sweep ]; then
  trap cleanup_netns EXIT INT TERM
  sweep "$2"
  exit 0
fi

require_root
prepare
mkdir -p "$OUT_DIR"
trap cleanup_netns EXIT INT TERM
for wire in $WIRES; do
  log "waiting for the bench lock ($LOCK) for $wire"
  flock "$LOCK" "$0" --sweep "$wire"
done
"$PY" "$BENCH/analyze_b3.py" "$OUT_DIR" "$RESULTS/b3_capacity$SUFFIX.json" --mtu "$MTU" \
  --server-cpu "$SERVER_CPU" --loadgen-cpus "$LOADGEN_CPUS"
