#!/usr/bin/env bash
# Jitter-buffer simulations: clock drift, bursts and delay steps, in simulated time.
#
# Runs every scenario of bench/jitter_scenarios.json through the server mixer's jitter
# buffer (server/internal/media TestJitterSimulation) and the client's
# (bench/jitter_sim.py), in simulated time. Deterministic: no root, no network and no
# bench lock. Each invocation writes to its own directory, bench/tmp/jitter/<LABEL>-<UTC>/,
# and bench/results/jitter_<LABEL>.json is made from that directory only.
#
# Usage: LABEL=before bench/run_jitter.sh
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"
trap 'exit 130' INT TERM

LABEL=${LABEL:?set LABEL, for example LABEL=before}
COMMAND="LABEL=$LABEL bench/run_jitter.sh"
RUNDIR="$TMPDIR_BENCH/jitter/$LABEL-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUNDIR" "$RESULTS"
cp "$BENCH/jitter_scenarios.json" "$RUNDIR/scenarios.json"
BIN="$RUNDIR/media.test"
log "building the media test binary"
(cd "$ROOT/server" && nice -n 19 go test -c -o "$BIN" ./internal/media)
write_build_record "$RUNDIR/build.json" "$BIN"
log "server mixer buffer"
(cd "$ROOT/server/internal/media" && CONFSERVER_JITTER_OUT="$RUNDIR/server.jsonl" \
  CONFSERVER_JITTER_SCENARIOS="$RUNDIR/scenarios.json" nice -n 19 "$BIN" \
  -test.run '^TestJitterSimulation$' -test.count 1 >"$RUNDIR/server.log" 2>&1)
log "client buffer"
nice -n 19 "$PY" "$BENCH/jitter_sim.py" client --scenarios "$RUNDIR/scenarios.json" \
  --out "$RUNDIR/client.jsonl"
"$PY" "$BENCH/jitter_sim.py" collect --rundir "$RUNDIR" --label "$LABEL" --command "$COMMAND" \
  --out "$RESULTS/jitter_$LABEL.json"
log "wrote $RESULTS/jitter_$LABEL.json"
