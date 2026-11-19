#!/usr/bin/env bash
# B3 relay capacity: how many participants (rooms of 4) each server sustains with
# >= 99 % frame delivery and p99 frame latency < 100 ms, and its CPU time per
# participant-minute at a fixed load.
#
# One network namespace, loopback only. The server is pinned to one CPU and the load
# generator (server/cmd/loadgen) to two others. Both systems get the same offered
# load: corpus frames (480x480 JPEG q85, ~31 kB) at 15 fps per participant plus 16 kHz
# audio, each in its own wire format (v2: RTP fragments + FEC; legacy: one pickled
# datagram per frame or audio chunk, sent from a distinct 127.1.x.y address per
# participant as the legacy relay requires).
#
# Loopback has no NIC: a UDP GSO message would go up the receivers' IP stack whole and
# be split only at the receiving socket, a receive path that a real network never
# offers and that is charged to the server's CPU (loopback receive work runs in
# softirq on the sending CPU). So lo's gso_max_segs is 1 by default: the kernel splits
# every GSO message before lo, as it does in front of a NIC without UDP segmentation
# offload, and every variant's datagrams take the same receive path. The run checks
# this with bench/lib/gso_split.py. GSO_MAX_SEGS=0 keeps lo's default (whole
# messages), which is how the first B3 runs were made.
#
# Every v2 variant (VARIANTS: name=flags, flags joined by commas) and the legacy relay
# run REPEATS times. Points are interleaved by repeat, size and variant, so slow drifts
# in the machine's load hit every variant alike. Within one repeat a variant stops
# after two consecutive failing sizes. Ctrl-C stops the run.
#
# Usage: sudo bench/run_b3.sh
#        WIRES=v2 VARIANTS="gso=" SIZES="16 32" REPEATS=1 FIXED_REPEATS=1 bench/run_b3.sh
# Environment, with defaults:
#   WIRES="v2 legacy"  VARIANTS="gso= nogso=-udp-gso=false nobatch=-udp-batch=1"
#   SIZES="128 144 ... 320"  LEGACY_SIZES=448  REPEATS=3  WINDOW=20s
#   FIXED=16  FIXED_REPEATS=3  FIXED_WINDOW=60s  MTU=65536  GSO_MAX_SEGS=1
#   SERVER_CPU=1  LOADGEN_CPUS=2,3  MAX_MBPS=12000  MAX_PPS=1500000  TAG=
#   CHUNK_SECONDS=720 (points run in chunks of at most this long under one hold of
#   the bench lock, which is released for a few seconds between chunks)
# Writes bench/results/b3_capacity[_mtuN][_TAG].json (or OUT) from this run's raw files
# only (bench/tmp/b3/<run ID>/).
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

WIRES=${WIRES:-"v2 legacy"}
VARIANTS=${VARIANTS:-"gso= nogso=-udp-gso=false nobatch=-udp-batch=1"}
SIZES=${SIZES:-"128 144 160 176 192 208 224 240 256 272 288 304 320"}
LEGACY_SIZES=${LEGACY_SIZES:-448}
REPEATS=${REPEATS:-3}
WINDOW=${WINDOW:-20s}
FIXED=${FIXED:-16} # participants for the CPU-per-participant-minute runs
FIXED_REPEATS=${FIXED_REPEATS:-3}
FIXED_WINDOW=${FIXED_WINDOW:-60s}
MTU=${MTU:-65536} # loopback MTU (65536 is the Linux default; 1500 = Ethernet)
GSO_MAX_SEGS=${GSO_MAX_SEGS:-1}
SERVER_CPU=${SERVER_CPU:-1}
LOADGEN_CPUS=${LOADGEN_CPUS:-2,3}
# The load generator's caps on its estimated total traffic (both directions, all
# headers); at 448 legacy or 320 v2 participants the load is about 7-9 Gbit/s.
MAX_MBPS=${MAX_MBPS:-12000}
MAX_PPS=${MAX_PPS:-1500000}
TAG=${TAG:-}
CHUNK_SECONDS=${CHUNK_SECONDS:-720} # longest hold of the bench lock (one chunk of points)
SUFFIX=""
[ "$MTU" != 65536 ] && SUFFIX="_mtu$MTU"
[ -n "$TAG" ] && SUFFIX="${SUFFIX}_$TAG"
OUT_DEFAULT=$RESULTS/b3_capacity$SUFFIX.json
OUT=${OUT:-$OUT_DEFAULT}
RUNDIR="$TMPDIR_BENCH/b3/$CS305_RUN_ID"
NS="${NS_PREFIX}b3"
COMMAND="WIRES=\"$WIRES\" VARIANTS=\"$VARIANTS\" SIZES=\"$SIZES\" LEGACY_SIZES=\"$LEGACY_SIZES\" REPEATS=$REPEATS WINDOW=$WINDOW FIXED=$FIXED FIXED_REPEATS=$FIXED_REPEATS FIXED_WINDOW=$FIXED_WINDOW MTU=$MTU GSO_MAX_SEGS=$GSO_MAX_SEGS SERVER_CPU=$SERVER_CPU LOADGEN_CPUS=$LOADGEN_CPUS MAX_MBPS=$MAX_MBPS MAX_PPS=$MAX_PPS TAG=$TAG CHUNK_SECONDS=$CHUNK_SECONDS bench/run_b3.sh"
# An output file other than the default is part of the command.
[ "$OUT" = "$OUT_DEFAULT" ] || COMMAND="OUT=$OUT $COMMAND"

setup_ns() {
  cleanup_netns
  ip netns add "$NS"
  ip -n "$NS" link set lo mtu "$MTU" up
  if [ "$GSO_MAX_SEGS" != 0 ]; then
    ip -n "$NS" link set lo gso_max_segs "$GSO_MAX_SEGS"
  fi
}

# variant_args NAME: the v2 server flags of a variant, space-separated.
variant_args() {
  local v
  for v in $VARIANTS; do
    if [ "${v%%=*}" = "$1" ]; then
      printf '%s' "${v#*=}" | tr ',' ' '
      return 0
    fi
  done
  echo "unknown variant $1" >&2
  return 1
}

# start_server WIRE LOG ARGS: start the server pinned to SERVER_CPU; sets SERVER_PID.
start_server() {
  local wire=$1 logf=$2 args=$3
  if [ "$wire" = v2 ]; then
    # shellcheck disable=SC2086 # ARGS is a list of flags
    ip netns exec "$NS" taskset -c "$SERVER_CPU" "$CONFSERVER" -control-addr 127.0.0.1:6100 \
      -media-addr 127.0.0.1:6100 -metrics-addr 127.0.0.1:6101 -log-level warn $args >"$logf" 2>&1 &
  else
    ip netns exec "$NS" taskset -c "$SERVER_CPU" "$PY" "$BENCH/legacy_harness/server.py" --ip 127.0.0.1 \
      >"$logf" 2>&1 &
  fi
  SERVER_PID=$! # ip netns exec runs the command directly, so this is the server itself
}

# run_point SYSTEM N WINDOW LABEL REPEAT: one load-generator run against a fresh
# server. Returns 0 if the size passes, 3 if it fails the criteria, 2 if the run itself
# failed twice.
run_point() {
  local system=$1 n=$2 window=$3 label=$4 rep=$5 wire=v2 args="" port=6100 attempt base out
  if [ "$system" = legacy ]; then
    wire=legacy port=8888
  else
    args=$(variant_args "${system#v2-}") || return 2
  fi
  base="$RUNDIR/$system-$n-$label-r$rep"
  out="$base.json"
  for attempt in 1 2; do
    rm -f "$out"
    setup_ns
    start_server "$wire" "$base.server.log" "$args"
    wait_tcp "$NS" 127.0.0.1 "$port" || true
    log "B3 $system participants=$n ($label, repeat $rep, window $window, lo mtu $MTU, gso_max_segs $GSO_MAX_SEGS)"
    if ! ip netns exec "$NS" taskset -c "$LOADGEN_CPUS" "$LOADGEN" -wire "$wire" -participants "$n" \
      -frames "$BENCH/data/frames" -duration "$window" -server-pid "$SERVER_PID" -server-core "$SERVER_CPU" \
      -label "$CS305_RUN_ID/$system/$label/r$rep" -max-mbps "$MAX_MBPS" -max-pps "$MAX_PPS" \
      -out "$out" 2>"$base.loadgen.log"; then
      log "loadgen failed for $system n=$n, attempt $attempt (see $base.loadgen.log)"
      rm -f "$out"
    fi
    if [ "$wire" = v2 ]; then
      scrape_metrics "$NS" >"$base.metrics"
    fi
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    cleanup_netns
    [ -f "$out" ] && break
  done
  [ -f "$out" ] || return 2
  "$PY" "$BENCH/analyze_b3.py" --check "$out"
}

systems=()
for wire in $WIRES; do
  if [ "$wire" = v2 ]; then
    for v in $VARIANTS; do systems+=("v2-${v%%=*}"); done
  else
    systems+=("$wire")
  fi
done

all_sizes=$(printf '%s\n' $SIZES $LEGACY_SIZES | sort -n | uniq)

# run_schedule: walk every point in order (repeat, size, system; then the fixed-load
# runs). A point that has a .status file was run in an earlier chunk and only feeds
# the stop rule; the others run until the chunk's time is up. Exits 0 when every point
# is done, 10 when the next one would not fit in this chunk, 130 when interrupted.
run_schedule() {
  local rep n system status est sf
  local -A fails stopped
  local deadline=$(($(date +%s) + CHUNK_SECONDS))
  for rep in $(seq 1 "$REPEATS"); do
    fails=() stopped=()
    for n in $all_sizes; do
      for system in "${systems[@]}"; do
        # v2 variants run over SIZES, the legacy relay over LEGACY_SIZES.
        if [ "$system" = legacy ]; then
          [[ " $LEGACY_SIZES " == *" $n "* ]] || continue
        else
          [[ " $SIZES " == *" $n "* ]] || continue
        fi
        [ -z "${stopped[$system]:-}" ] || continue
        sf="$RUNDIR/$system-$n-sweep-r$rep.status"
        if [ -f "$sf" ]; then
          status=$(cat "$sf")
        else
          est=$((${WINDOW%s} + 12 + n / 40)) # warm-up 5 s, grace 1 s, setup and teardown
          [ $(($(date +%s) + est)) -le "$deadline" ] || return 10
          status=0
          run_point "$system" "$n" "$WINDOW" sweep "$rep" || status=$?
          [ "$status" -lt 128 ] || return 130
          echo "$status" >"$sf"
        fi
        case $status in
          0) fails[$system]=0 ;;
          2) stopped[$system]=1 ;; # the run itself failed twice
          *)
            fails[$system]=$((${fails[$system]:-0} + 1))
            [ "${fails[$system]}" -lt 2 ] || stopped[$system]=1
            ;;
        esac
      done
    done
  done
  for rep in $(seq 1 "$FIXED_REPEATS"); do
    for system in "${systems[@]}"; do
      sf="$RUNDIR/$system-$FIXED-fixed-r$rep.status"
      [ ! -f "$sf" ] || continue
      est=$((${FIXED_WINDOW%s} + 12 + FIXED / 40))
      [ $(($(date +%s) + est)) -le "$deadline" ] || return 10
      status=0
      run_point "$system" "$FIXED" "$FIXED_WINDOW" fixed "$rep" || status=$?
      [ "$status" -lt 128 ] || return 130
      echo "$status" >"$sf"
    done
  done
  return 0
}

if [ "${1:-}" = --chunk ]; then
  # Inner step, under the bench lock: points until CHUNK_SECONDS are used up.
  trap cleanup_netns EXIT
  trap 'exit 130' INT TERM
  status=0
  run_schedule || status=$?
  exit "$status"
fi

if [ "${1:-}" = --gso-check ]; then
  trap cleanup_netns EXIT
  trap 'exit 130' INT TERM
  setup_ns
  ip netns exec "$NS" "$PY" "$BENCH/lib/gso_split.py"
  exit 0
fi

require_root
trap cleanup_netns EXIT # only this run's namespaces
trap 'exit 130' INT TERM
if [ -f "$RUNDIR/settings.json" ]; then
  # Resuming a run (CS305_RUN_ID and CS305_INVOCATION set to the stopped run's): the
  # same binaries and settings, and the points with a .status file are not repeated.
  "$PY" -c 'import json, sys; sys.exit(json.load(open(sys.argv[1]))["command"] != sys.argv[2])' \
    "$RUNDIR/settings.json" "sudo $COMMAND" || {
    echo "run $CS305_RUN_ID was started with other settings" >&2
    exit 1
  }
  [ -x "$CONFSERVER" ] && [ -x "$LOADGEN" ] || {
    echo "the binaries of run $CS305_RUN_ID are gone" >&2
    exit 1
  }
  log "resuming run $CS305_RUN_ID"
  resume=1
else
  prepare
  mkdir -p "$RUNDIR"
  log "run $CS305_RUN_ID: raw files in $RUNDIR"
  "$0" --gso-check >"$RUNDIR/lo_gso_check.json"
  resume=0
fi
[ "$resume" = 1 ] || {
  printf '{"run_id": "%s", "command": "sudo %s",\n' "$CS305_RUN_ID" "$(printf '%s' "$COMMAND" | sed 's/"/\\"/g')"
  printf ' "wires": "%s", "variants": "%s", "sizes": "%s", "legacy_sizes": "%s", "repeats": %s,\n' \
    "$WIRES" "$VARIANTS" "$SIZES" "$LEGACY_SIZES" "$REPEATS"
  printf ' "window": "%s", "fixed": %s, "fixed_repeats": %s, "fixed_window": "%s", "loopback_mtu": %s,\n' \
    "$WINDOW" "$FIXED" "$FIXED_REPEATS" "$FIXED_WINDOW" "$MTU"
  printf ' "lo_gso_max_segs": %s, "server_cpu": "%s", "loadgen_cpus": "%s", "max_mbps": %s, "max_pps": %s}\n' \
    "$GSO_MAX_SEGS" "$SERVER_CPU" "$LOADGEN_CPUS" "$MAX_MBPS" "$MAX_PPS"
} >"$RUNDIR/settings.json"

chunk=0
while :; do
  chunk=$((chunk + 1))
  log "chunk $chunk: waiting for the bench lock ($LOCK)"
  status=0
  locked "$0" --chunk || status=$?
  case $status in
    0) break ;;
    10) sleep 5 ;; # let others take the lock between chunks
    *)
      log "stopped (status $status)"
      exit "$status"
      ;;
  esac
done
"$PY" "$BENCH/analyze_b3.py" "$RUNDIR" "$OUT" --build "$BUILD_DIR/build.json"
