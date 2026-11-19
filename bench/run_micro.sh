#!/usr/bin/env bash
# Go microbenchmarks of the server's hot paths (go test -bench . -benchmem), COUNT runs
# each, pinned to one CPU under the shared bench lock.
#
# The send benchmarks (udpio BenchmarkSendFrame, media BenchmarkForwardVideo) use
# loopback receivers: their times include the receivers' IP receive path, which on lo
# runs in softirq on the sending CPU, and a UDP GSO message goes up that path whole. So
# their GSO speed-ups are loopback-only upper bounds (see bench/run_b3.sh for the
# setting that splits GSO messages before lo).
#
# Usage: bench/run_micro.sh          (no root needed)
# Environment: COUNT (5), MICRO_CPU (1).
# Writes bench/results/micro.json (median, minimum and maximum of the runs) and the raw
# output bench/results/micro.txt; the test binaries and their build record stay in
# bench/tmp/micro/<run ID>/.
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"
trap 'exit 130' INT TERM

COUNT=${COUNT:-5}
CPU=${MICRO_CPU:-1}
COMMAND="COUNT=$COUNT MICRO_CPU=$CPU bench/run_micro.sh"
RUNDIR="$TMPDIR_BENCH/micro/$CS305_RUN_ID"
mkdir -p "$RUNDIR" "$RESULTS"
log "building the test binaries into $RUNDIR"
for pkg in protocol media udpio; do
  (cd "$ROOT/server" && GOFLAGS="${GOFLAGS:--p=2}" nice -n 19 go test -c -o "$RUNDIR/$pkg.test" "./internal/$pkg")
done
write_build_record "$RUNDIR/build.json" "$RUNDIR/protocol.test" "$RUNDIR/media.test" "$RUNDIR/udpio.test"
log "waiting for the bench lock ($LOCK)"
locked bash -c "
  set -euo pipefail
  for pkg in protocol media udpio; do
    cd '$ROOT/server/internal/'\$pkg
    taskset -c $CPU '$RUNDIR/'\$pkg.test -test.run '^\$' -test.bench . -test.benchmem -test.count $COUNT -test.cpu 1
  done
" >"$RUNDIR/micro.txt"
cp "$RUNDIR/micro.txt" "$RESULTS/micro.txt"
"$PY" "$BENCH/analyze_micro.py" "$RUNDIR/micro.txt" "$RESULTS/micro.json" --build "$RUNDIR/build.json" \
  --command "$COMMAND" --count "$COUNT" --cpu "$CPU"
