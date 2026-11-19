#!/usr/bin/env bash
# B2 multi-talker audio: 3- and 5-party calls in which everybody talks and sends video.
#
# All participants and the server run in one network namespace on its loopback
# interface; each legacy participant has its own 127.0.0.x address (the legacy relay
# skips receivers with the sender's IP). Legacy: the original relay and the original
# keep_share/keep_recv threads with a sound-card-clocked fake PyAudio (see
# bench/legacy_harness/stubs.py). v2: the Go server (mix-minus) and the real client.
#
# Usage: sudo bench/run_b2.sh            (both systems, 3 and 5 parties, 60 s each)
#        SYSTEMS=v2 SIZES=3 MEASURE_SECONDS=10 bench/run_b2.sh
# Writes bench/results/b2_audio.json. The whole sweep holds the shared bench lock.
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

SYSTEMS=${SYSTEMS:-"legacy v2"}
SIZES=${SIZES:-"3 5"}
MEASURE_SECONDS=${MEASURE_SECONDS:-60}
OUT_DIR="$TMPDIR_BENCH/b2"
NS=${NS_PREFIX}2

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

run_one() {
  local system=$1 n=$2 tag="$1-$2" server_pid end_at start_at i pids=()
  setup_ns
  log "B2 $system with $n participants"
  if [ "$system" = legacy ]; then
    ip netns exec "$NS" "$PY" "$BENCH/legacy_harness/server.py" --ip 127.0.0.1 >"$OUT_DIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$NS" 127.0.0.1 8888
  else
    ip netns exec "$NS" "$CONFSERVER" -control-addr 127.0.0.1:6100 -media-addr 127.0.0.1:6100 \
      -metrics-addr 127.0.0.1:6101 >"$OUT_DIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$NS" 127.0.0.1 6100
  fi
  end_at=$(mono_plus "$((MEASURE_SECONDS + 3 + n))")
  for i in $(seq 1 "$n"); do
    local talker=$((i - 1))
    if [ "$system" = legacy ]; then
      local role=()
      [ "$i" -eq 1 ] && role=(--create)
      ip netns exec "$NS" "$PY" "$BENCH/legacy_harness/client.py" --id "$i" --ip "127.0.0.$((10 + i))" \
        --server-ip 127.0.0.1 "${role[@]}" --send --talker "$talker" --talkers "$n" --end-at "$end_at" \
        --out "$OUT_DIR/$tag.p$i.json" >"$OUT_DIR/$tag.p$i.log" 2>&1 &
    else
      local role=(--join 1)
      [ "$i" -eq 1 ] && role=(--create b2)
      ip netns exec "$NS" "$PY" "$BENCH/v2_client.py" --server 127.0.0.1 --name "p$i" "${role[@]}" \
        --send-video --send-audio --talker "$talker" --talkers "$n" --end-at "$end_at" \
        --out "$OUT_DIR/$tag.p$i.json" >"$OUT_DIR/$tag.p$i.log" 2>&1 &
    fi
    pids+=($!)
    sleep 0.5
  done
  sleep 2.5
  start_at=$(mono)
  local nft_start
  nft_start=$(nft_counters)
  "$PY" -c "import time; time.sleep(max(0.0, $end_at - 1.0 - time.monotonic()))"
  local nft_end window_end
  window_end=$(mono)
  nft_end=$(nft_counters)
  wait "${pids[@]}"
  printf '{"system": "%s", "participants": %s, "window_start": %s, "window_end": %s, "nft_start": %s, "nft_end": %s, "snmp": %s}\n' \
    "$system" "$n" "$start_at" "$window_end" "$nft_start" "$nft_end" "$(snmp "$NS")" >"$OUT_DIR/$tag.meta.json"
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  cleanup_netns
}

if [ "${1:-}" = --sweep ]; then
  trap cleanup_netns EXIT INT TERM
  for system in $SYSTEMS; do
    for n in $SIZES; do run_one "$system" "$n"; done
  done
  exit 0
fi

require_root
prepare
mkdir -p "$OUT_DIR"
trap cleanup_netns EXIT INT TERM
log "waiting for the bench lock ($LOCK)"
flock "$LOCK" "$0" --sweep
"$PY" "$BENCH/analyze_b2.py" "$OUT_DIR" "$RESULTS/b2_audio.json"
