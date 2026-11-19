# Chunked, resumable schedules for the benchmark scripts (sourced after common.sh).
#
# A benchmark walks its steps in a fixed order. A step that has run leaves
# RUNDIR/STEP.status with its exit status, so a later chunk (or a resumed run) skips
# it. The outer process calls run_chunks, which re-executes "$0 --chunk" under the
# bench lock until every step is done; a chunk runs steps until the next one would not
# finish within CHUNK_SECONDS of the chunk's start, then releases the lock for a few
# seconds so that other benchmarks can take it.

CHUNK_SECONDS=${CHUNK_SECONDS:-720}

chunk_begin() { CHUNK_DEADLINE=$(($(date +%s) + CHUNK_SECONDS)); }

# step NAME EST CMD...: run CMD as step NAME unless it has already run. Returns the
# step's status (0 or a failure below 10, recorded), 10 when a step of EST seconds does
# not fit in this chunk, or 130 when CMD was interrupted.
step() {
  local name=$1 est=$2 sf status=0
  shift 2
  sf="$RUNDIR/$name.status"
  if [ -f "$sf" ]; then
    return "$(cat "$sf")"
  fi
  [ $(($(date +%s) + est)) -le "$CHUNK_DEADLINE" ] || return 10
  "$@" || status=$?
  [ "$status" -lt 128 ] || return 130
  [ "$status" -lt 10 ] || status=9 # 10 and up mean something else to the caller
  echo "$status" >"$sf"
  return "$status"
}

# run_chunks: run "$0 --chunk" under the lock until it reports that everything is done.
run_chunks() {
  local chunk=0 status
  while :; do
    chunk=$((chunk + 1))
    log "chunk $chunk: waiting for the bench lock ($LOCK)"
    status=0
    locked "$0" --chunk || status=$?
    case $status in
      0) return 0 ;;
      10) sleep 5 ;;
      *)
        log "stopped (status $status)"
        return "$status"
        ;;
    esac
  done
}
