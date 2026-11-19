#!/usr/bin/env bash
# Forwarding latency while the control plane churns (review finding server-1).
#
# Builds the server/internal/media test binary once, then runs TestChurnForwarding RUNS
# times. Each run takes the bench lock on its own (five configurations of
# SECONDS_PER_CONFIG each; about 110 s with the defaults),
# pinned to two CPUs with GOMAXPROCS=2. The raw JSON lines and the build record go to a
# fresh directory per invocation, bench/tmp/churn/<LABEL>-<UTC>/, and
# bench/analyze_churn.py writes bench/results/churn_<LABEL>.json from that directory only.
#
# Usage: LABEL=before bench/run_churn.sh      (no root needed; loopback UDP only)
# Environment: LABEL (required), RUNS (3), SECONDS_PER_CONFIG (20), CHURN_CPUS (1,2).
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"
trap 'exit 130' INT TERM

LABEL=${LABEL:?set LABEL, for example LABEL=before}
RUNS=${RUNS:-3}
SECONDS_PER_CONFIG=${SECONDS_PER_CONFIG:-20}
CPUS=${CHURN_CPUS:-1,2}
COMMAND="LABEL=$LABEL RUNS=$RUNS SECONDS_PER_CONFIG=$SECONDS_PER_CONFIG CHURN_CPUS=$CPUS bench/run_churn.sh"

RUNDIR="$TMPDIR_BENCH/churn/$LABEL-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUNDIR" "$RESULTS"
BIN="$RUNDIR/media.test"
log "building the media test binary"
(cd "$ROOT/server" && nice -n 19 go test -c -o "$BIN" ./internal/media)
write_build_record "$RUNDIR/build.json" "$BIN"

for i in $(seq 1 "$RUNS"); do
  log "run $i/$RUNS: waiting for the bench lock ($LOCK)"
  flock "$LOCK" env GOMAXPROCS=2 CONFSERVER_CHURN_OUT="$RUNDIR/run$i.jsonl" \
    CHURN_SECONDS="$SECONDS_PER_CONFIG" taskset -c "$CPUS" "$BIN" \
    -test.run '^TestChurnForwarding$' -test.count 1 -test.v >"$RUNDIR/run$i.log" 2>&1
done
"$PY" "$BENCH/analyze_churn.py" --rundir "$RUNDIR" --label "$LABEL" --command "$COMMAND" \
  --out "$RESULTS/churn_$LABEL.json"
log "wrote $RESULTS/churn_$LABEL.json"
