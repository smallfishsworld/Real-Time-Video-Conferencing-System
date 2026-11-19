#!/usr/bin/env bash
# B2 multi-talker audio: 3- and 5-party calls in which everybody talks and sends video.
#
# Both ends are simulated. Every participant's microphone is a synthetic tone carrying
# a tag (talker, sequence number, capture time), and its speaker logs the tags it
# plays. All participants and the server run in one network namespace on its loopback
# interface; each legacy participant has its own 127.0.0.x address (the legacy relay
# skips receivers with the sender's IP).
#   legacy  the original relay and the original keep_share/keep_recv threads. The
#           original's PyAudio streams are replaced by stand-ins (bench/legacy_harness/
#           stubs.py): the microphone returns a chunk once its duration has passed, and
#           the speaker is a blocking write with room for one chunk, so a receive thread
#           that writes faster than real time blocks for one chunk's duration per write.
#           Its latency figures depend on that stand-in.
#   v2      the Go server (mix-minus) and the real client's media code; the speaker is
#           a thread that pulls one 20 ms frame every 20 ms, like a sound card
#           (bench/v2_client.py).
# Latency is capture-to-playout: from the end of a chunk's capture at the talker to the
# moment the receiver's (simulated) speaker starts playing it. All processes share
# CLOCK_MONOTONIC.
#
# Every (system, size) runs REPEATS times, interleaved by repeat, in chunks of at most
# CHUNK_SECONDS under the bench lock. Ctrl-C stops the run; CS305_RUN_ID and
# CS305_INVOCATION set to a stopped run's resume it.
#
# Usage: sudo bench/run_b2.sh            (both systems, 3 and 5 parties, 3 x 60 s each)
#        SYSTEMS=v2 SIZES=3 REPEATS=1 MEASURE_SECONDS=10 bench/run_b2.sh
# Environment, with defaults: SYSTEMS="legacy v2"  SIZES="3 5"  REPEATS=3
#   MEASURE_SECONDS=60  CHUNK_SECONDS=720
# Writes bench/results/b2_audio.json (or OUT) from this run's files (bench/tmp/b2/<run ID>/).
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/schedule.sh"

SYSTEMS=${SYSTEMS:-"legacy v2"}
SIZES=${SIZES:-"3 5"}
REPEATS=${REPEATS:-3}
MEASURE_SECONDS=${MEASURE_SECONDS:-60}
OUT=${OUT:-$RESULTS/b2_audio.json}
RUNDIR="$TMPDIR_BENCH/b2/$CS305_RUN_ID"
NS="${NS_PREFIX}b2"
COMMAND="SYSTEMS=\"$SYSTEMS\" SIZES=\"$SIZES\" REPEATS=$REPEATS MEASURE_SECONDS=$MEASURE_SECONDS CHUNK_SECONDS=$CHUNK_SECONDS bench/run_b2.sh"

setup_ns() {
  cleanup_netns
  ip netns add "$NS"
  ip -n "$NS" link set lo up
  # Kernel-side byte counters per legacy receiver port, split by size: legacy audio
  # datagrams are about 2.1 kB, video datagrams about 31 kB.
  {
    echo "table ip cs305b2 {"
    for id in 1 2 3 4 5; do
      echo "  counter a$id {}"
      echo "  counter v$id {}"
    done
    echo "  chain in {"
    echo "    type filter hook input priority 0; policy accept;"
    for id in 1 2 3 4 5; do
      echo "    udp dport $((20000 + id * 100)) meta length < 4000 counter name a$id"
      echo "    udp dport $((20000 + id * 100)) meta length >= 4000 counter name v$id"
    done
    echo "  }"
    echo "}"
  } | ip netns exec "$NS" nft -f -
}

nft_counters() {
  ip netns exec "$NS" nft -j list counters | "$PY" -c '
import json, sys
data = json.load(sys.stdin)["nftables"]
print(json.dumps({c["counter"]["name"]: [c["counter"]["packets"], c["counter"]["bytes"]] for c in data if "counter" in c}))'
}

# run_one SYSTEM N REPEAT: one call; its files are RUNDIR/TAG.*. Returns non-zero if
# any process failed (then no .meta.json is written).
run_one() {
  local system=$1 n=$2 rep=$3 tag="$1-$2-r$3" server_pid end_at start_at i pids=() status=0
  setup_ns
  log "B2 $system with $n participants, repeat $rep (${MEASURE_SECONDS}s)"
  if [ "$system" = legacy ]; then
    ip netns exec "$NS" "$PY" "$BENCH/legacy_harness/server.py" --ip 127.0.0.1 >"$RUNDIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$NS" 127.0.0.1 8888 || return 1
  else
    ip netns exec "$NS" "$CONFSERVER" -control-addr 127.0.0.1:6100 -media-addr 127.0.0.1:6100 \
      -metrics-addr 127.0.0.1:6101 >"$RUNDIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$NS" 127.0.0.1 6100 || return 1
  fi
  end_at=$(mono_plus "$((MEASURE_SECONDS + 3 + n))")
  for i in $(seq 1 "$n"); do
    local talker=$((i - 1))
    if [ "$system" = legacy ]; then
      local role=()
      [ "$i" -ne 1 ] || role=(--create)
      ip netns exec "$NS" "$PY" "$BENCH/legacy_harness/client.py" --id "$i" --ip "127.0.0.$((10 + i))" \
        --server-ip 127.0.0.1 "${role[@]}" --send --talker "$talker" --talkers "$n" --end-at "$end_at" \
        --out "$RUNDIR/$tag.p$i.json" >"$RUNDIR/$tag.p$i.log" 2>&1 &
    else
      local role=(--join 1)
      [ "$i" -ne 1 ] || role=(--create b2)
      ip netns exec "$NS" "$PY" "$BENCH/v2_client.py" --server 127.0.0.1 --name "p$i" "${role[@]}" \
        --send-video --send-audio --talker "$talker" --talkers "$n" --end-at "$end_at" \
        --out "$RUNDIR/$tag.p$i.json" >"$RUNDIR/$tag.p$i.log" 2>&1 &
    fi
    pids+=($!)
    sleep 0.5
  done
  sleep 2.5
  start_at=$(mono)
  local nft_start nft_end window_end
  nft_start=$(nft_counters)
  "$PY" -c "import time; time.sleep(max(0.0, $end_at - 1.0 - time.monotonic()))"
  window_end=$(mono)
  nft_end=$(nft_counters)
  wait_all "${pids[@]}" || status=$?
  if [ "$status" -eq 0 ]; then
    printf '{"run_id": "%s", "system": "%s", "participants": %s, "repeat": %s, "window_start": %s, "window_end": %s, "nft_start": %s, "nft_end": %s, "snmp": %s}\n' \
      "$CS305_RUN_ID" "$system" "$n" "$rep" "$start_at" "$window_end" "$nft_start" "$nft_end" "$(snmp "$NS")" \
      >"$RUNDIR/$tag.meta.json"
  else
    log "a process of $tag failed (status $status); see $RUNDIR/$tag.*.log"
  fi
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  cleanup_netns
  return "$status"
}

# run_schedule: every run in order; see lib/schedule.sh.
run_schedule() {
  local rep system n s
  chunk_begin
  for rep in $(seq 1 "$REPEATS"); do
    for n in $SIZES; do
      for system in $SYSTEMS; do
        s=0
        step "$system-$n-r$rep" "$((MEASURE_SECONDS + 3 + n + 10))" run_one "$system" "$n" "$rep" || s=$?
        case $s in 10 | 130) return "$s" ;; esac
      done
    done
  done
  return 0
}

if [ "${1:-}" = --chunk ]; then
  trap cleanup_netns EXIT
  trap 'exit 130' INT TERM
  status=0
  run_schedule || status=$?
  exit "$status"
fi

require_root
trap cleanup_netns EXIT # only this run's namespaces
trap 'exit 130' INT TERM
if [ -f "$RUNDIR/command.txt" ]; then
  [ "$(cat "$RUNDIR/command.txt")" = "sudo $COMMAND" ] || {
    echo "run $CS305_RUN_ID was started with other settings" >&2
    exit 1
  }
  log "resuming run $CS305_RUN_ID"
else
  prepare
  mkdir -p "$RUNDIR"
  printf '%s\n' "sudo $COMMAND" >"$RUNDIR/command.txt"
  log "run $CS305_RUN_ID: raw files in $RUNDIR"
fi
run_chunks
"$PY" "$BENCH/analyze_b2.py" "$RUNDIR" "$OUT" --build "$BUILD_DIR/build.json" --command "$(cat "$RUNDIR/command.txt")"
