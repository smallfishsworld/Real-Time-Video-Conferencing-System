#!/usr/bin/env bash
# Go microbenchmarks of the server's hot paths (go test -bench . -benchmem), five runs
# each, pinned to one CPU under the shared bench lock.
#
# Usage: bench/run_micro.sh          (no root needed)
# Writes bench/results/micro.json (median of the runs) and keeps the raw output in
# bench/results/micro.txt.
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

COUNT=${COUNT:-5}
CPU=${MICRO_CPU:-1}
mkdir -p "$RESULTS"
RAW="$RESULTS/micro.txt"
log "waiting for the bench lock ($LOCK)"
(cd "$ROOT/server" && nice -n 19 go test -c -o "$TMPDIR_BENCH/protocol.test" ./internal/protocol &&
  nice -n 19 go test -c -o "$TMPDIR_BENCH/media.test" ./internal/media &&
  nice -n 19 go test -c -o "$TMPDIR_BENCH/udpio.test" ./internal/udpio)
flock "$LOCK" bash -c "
  set -euo pipefail
  cd '$ROOT/server/internal/protocol'
  taskset -c $CPU '$TMPDIR_BENCH/protocol.test' -test.run '^\$' -test.bench . -test.benchmem -test.count $COUNT -test.cpu 1
  cd '$ROOT/server/internal/media'
  taskset -c $CPU '$TMPDIR_BENCH/media.test' -test.run '^\$' -test.bench . -test.benchmem -test.count $COUNT -test.cpu 1
  cd '$ROOT/server/internal/udpio'
  taskset -c $CPU '$TMPDIR_BENCH/udpio.test' -test.run '^\$' -test.bench . -test.benchmem -test.count $COUNT -test.cpu 1
" >"$RAW"
"$PY" "$BENCH/analyze_micro.py" "$RAW" "$RESULTS/micro.json"
