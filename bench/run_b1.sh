#!/usr/bin/env bash
# B1 loss resilience: video frame delivery and audio loss under random packet loss.
#
#   sender ns ──veth (MTU 1500)── server ns ──veth (MTU 1500)── receiver ns
#
# Conditions:
#   downlink    the best case for recovery: nftables in the receiver's namespace drops
#               N per mille of UDP packets at prerouting, priority -500 (before IPv4
#               defragmentation, so single IP fragments are lost, as on a lossy link),
#               server -> receiver only. The receiver's NACKs and REGISTERs take an
#               unimpaired path with a sub-millisecond round trip.
#   both-rtt50  harder, v2 only: the receiver's media goes through a userspace relay in
#               the server's namespace (bench/lib/udp_impair.py, reached through DNAT in
#               the receiver's namespace) that drops N per mille of datagrams
#               independently in each direction and delays each direction by
#               HARD_DELAY_MS (25 ms: +50 ms round trip). NACKs, retransmissions, parity
#               and REGISTERs can all be lost. This kernel has no netem, hence the
#               relay. It works on whole datagrams, so the original (31 kB datagrams sent
#               as IP fragments) is not run in this condition.
#   downlink-long  the downlink condition for the original only, LONG_SECONDS per run at
#               LONG_LOSSES, to show how its delivery changes as the receiver's IP
#               reassembly queue fills.
# Systems: the original code (legacy relay + legacy client media threads), v2 without
# recovery, v2 with FEC (video k=4, audio k=2), v2 with NACK retransmission of video,
# and v2 with both (the client's default). P2P is off on the v2 server, so both systems
# relay through the server (legacy: relay mode, as `chgmode` selects).
#
# Every (condition, loss rate, system) runs REPEATS times, interleaved by repeat, loss
# rate and system, in chunks of at most CHUNK_SECONDS under the bench lock. Ctrl-C
# stops the run; CS305_RUN_ID and CS305_INVOCATION set to a stopped run's resume it.
#
# Usage: sudo bench/run_b1.sh
#        CONDITIONS=downlink SYSTEMS=v2-fec LOSSES=50 REPEATS=1 SECONDS_PER_RUN=10 bench/run_b1.sh
# Environment, with defaults:
#   CONDITIONS="downlink both-rtt50 downlink-long"  REPEATS=3  SECONDS_PER_RUN=41
#   SYSTEMS="legacy v2-nofec v2-fec v2-nack v2-fec-nack"  LOSSES="0 10 20 50 100"
#   HARD_SYSTEMS="v2-fec v2-nack v2-fec-nack"  HARD_LOSSES="0 20 50 100"  HARD_DELAY_MS=25
#   LONG_LOSSES=10  LONG_SECONDS=300  LONG_REPEATS=1  CHUNK_SECONDS=720
#   RELAY_LOG=0 (1: the relay also logs every datagram to TAG.relay.jsonl, for diagnosis)
# Writes bench/results/b1_loss.json (or OUT) from this run's files (bench/tmp/b1/<run ID>/).
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"
source "$(dirname "$0")/lib/schedule.sh"

CONDITIONS=${CONDITIONS:-"downlink both-rtt50 downlink-long"}
REPEATS=${REPEATS:-3}
SECONDS_PER_RUN=${SECONDS_PER_RUN:-41}
SYSTEMS=${SYSTEMS:-"legacy v2-nofec v2-fec v2-nack v2-fec-nack"}
LOSSES=${LOSSES:-"0 10 20 50 100"} # per mille
HARD_SYSTEMS=${HARD_SYSTEMS:-"v2-fec v2-nack v2-fec-nack"}
HARD_LOSSES=${HARD_LOSSES:-"0 20 50 100"}
HARD_DELAY_MS=${HARD_DELAY_MS:-25}
LONG_LOSSES=${LONG_LOSSES:-10}
LONG_SECONDS=${LONG_SECONDS:-300}
LONG_REPEATS=${LONG_REPEATS:-1}
RELAY_LOG=${RELAY_LOG:-0}
OUT_DEFAULT=$RESULTS/b1_loss.json
OUT=${OUT:-$OUT_DEFAULT}
RUNDIR="$TMPDIR_BENCH/b1/$CS305_RUN_ID"
SRV="${NS_PREFIX}b1srv" SND="${NS_PREFIX}b1snd" RCV="${NS_PREFIX}b1rcv"
SERVER_IP=10.77.0.1 SND_IP=10.77.1.2 RCV_IP=10.77.2.2 RELAY=10.77.2.1:7000
COMMAND="CONDITIONS=\"$CONDITIONS\" REPEATS=$REPEATS SECONDS_PER_RUN=$SECONDS_PER_RUN SYSTEMS=\"$SYSTEMS\" LOSSES=\"$LOSSES\" HARD_SYSTEMS=\"$HARD_SYSTEMS\" HARD_LOSSES=\"$HARD_LOSSES\" HARD_DELAY_MS=$HARD_DELAY_MS LONG_LOSSES=\"$LONG_LOSSES\" LONG_SECONDS=$LONG_SECONDS LONG_REPEATS=$LONG_REPEATS RELAY_LOG=$RELAY_LOG CHUNK_SECONDS=$CHUNK_SECONDS bench/run_b1.sh"
# An output file other than the default is part of the command.
[ "$OUT" = "$OUT_DEFAULT" ] || COMMAND="OUT=$OUT $COMMAND"

# setup_ns CONDITION LOSS: the three namespaces, the veth links and the impairment.
setup_ns() {
  local cond=$1 loss=$2 drop=$2
  cleanup_netns
  ip netns add "$SRV"
  ip netns add "$SND"
  ip netns add "$RCV"
  for ns in "$SRV" "$SND" "$RCV"; do ip -n "$ns" link set lo up; done
  ip -n "$SRV" addr add "$SERVER_IP/32" dev lo
  ip link add b1s0 netns "$SND" type veth peer name b1s1 netns "$SRV"
  ip link add b1r0 netns "$RCV" type veth peer name b1r1 netns "$SRV"
  ip -n "$SND" link set b1s0 mtu 1500 up
  ip -n "$SRV" link set b1s1 mtu 1500 up
  ip -n "$RCV" link set b1r0 mtu 1500 up
  ip -n "$SRV" link set b1r1 mtu 1500 up
  # The v2 server sends forwarded video as UDP GSO messages, which a real NIC (or the
  # kernel, just before the driver) splits into datagrams before they reach the wire.
  # veth would carry them whole into the receiver namespace, where the loss rule would
  # then drop a whole message at a time; one segment per packet on the server's side
  # makes the kernel split them as it transmits, so the rule sees single datagrams.
  ip -n "$SRV" link set b1r1 gso_max_segs 1
  ip -n "$SND" addr add "$SND_IP/24" dev b1s0
  ip -n "$SRV" addr add 10.77.1.1/24 dev b1s1
  ip -n "$RCV" addr add "$RCV_IP/24" dev b1r0
  ip -n "$SRV" addr add 10.77.2.1/24 dev b1r1
  ip -n "$SND" route add 10.77.0.0/24 via 10.77.1.1
  ip -n "$RCV" route add 10.77.0.0/24 via 10.77.2.1
  if [ "$cond" = both-rtt50 ]; then
    drop=0 # the relay drops instead
  fi
  ip netns exec "$RCV" nft -f - <<EOF
table ip cs305loss {
  counter udp_in {}
  counter udp_drop {}
  chain pre {
    type filter hook prerouting priority -500; policy accept;
    ip protocol udp counter name udp_in
    ip protocol udp numgen random mod 1000 < $drop counter name udp_drop drop
  }
}
EOF
  if [ "$cond" = both-rtt50 ]; then
    # The receiver's media to the server goes to the relay; conntrack maps the replies.
    ip netns exec "$RCV" nft -f - <<EOF
table ip cs305relay {
  chain out {
    type nat hook output priority -100; policy accept;
    ip daddr $SERVER_IP udp dport 6100 dnat to $RELAY
  }
}
EOF
  fi
}

nft_counters() {
  ip netns exec "$RCV" nft -j list counters | "$PY" -c '
import json, sys
data = json.load(sys.stdin)["nftables"]
print(json.dumps({c["counter"]["name"]: c["counter"]["packets"] for c in data if "counter" in c}))'
}

# run_one CONDITION SYSTEM LOSS REPEAT SECONDS: one run; its files are RUNDIR/TAG.*.
# Returns non-zero if any process of the run failed (then no .net.json is written).
run_one() {
  local cond=$1 system=$2 loss=$3 rep=$4 secs=$5 tag="$1-$2-$3-r$4" end_at server_pid recv_pid send_pid
  local frag_pid relay_pid="" pids=() status=0 net=${1%-long} relay_log=()
  setup_ns "$net" "$loss"
  log "B1 $cond $system loss=${loss}/1000 repeat $rep (${secs}s)"
  if [ "$system" = legacy ]; then
    ip netns exec "$SRV" "$PY" "$BENCH/legacy_harness/server.py" --ip "$SERVER_IP" >"$RUNDIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$SRV" "$SERVER_IP" 8888 || return 1
    end_at=$(mono_plus "$secs")
    ip netns exec "$RCV" "$PY" "$BENCH/legacy_harness/client.py" --id 2 --ip "$RCV_IP" --server-ip "$SERVER_IP" \
      --create --end-at "$end_at" --out "$RUNDIR/$tag.recv.json" >"$RUNDIR/$tag.recv.log" 2>&1 &
    recv_pid=$!
    sleep 1
    ip netns exec "$SND" "$PY" "$BENCH/legacy_harness/client.py" --id 1 --ip "$SND_IP" --server-ip "$SERVER_IP" \
      --send --end-at "$end_at" --out "$RUNDIR/$tag.send.json" >"$RUNDIR/$tag.send.log" 2>&1 &
    send_pid=$!
  else
    local fec_v=4 fec_a=2 nack=""
    case $system in v2-nofec | v2-nack) fec_v=0 fec_a=0 ;; esac
    case $system in *nack) nack=--nack ;; esac
    ip netns exec "$SRV" "$CONFSERVER" -control-addr "$SERVER_IP:6100" -media-addr "$SERVER_IP:6100" \
      -metrics-addr 127.0.0.1:6101 -p2p=false -audio-fec-k "$fec_a" >"$RUNDIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$SRV" "$SERVER_IP" 6100 || return 1
    end_at=$(mono_plus "$secs")
    if [ "$net" = both-rtt50 ]; then
      [ "$RELAY_LOG" = 0 ] || relay_log=(--log "$RUNDIR/$tag.relay.jsonl")
      ip netns exec "$SRV" "$PY" "$BENCH/lib/udp_impair.py" --listen "$RELAY" --upstream "$SERVER_IP:6100" \
        --delay-ms "$HARD_DELAY_MS" --loss "$("$PY" -c "print($loss / 1000)")" --seed "$((rep * 1000 + loss))" \
        --end-at "$end_at" --stats "$RUNDIR/$tag.relay.json" "${relay_log[@]}" >"$RUNDIR/$tag.relay.log" 2>&1 &
      relay_pid=$!
      sleep 0.3
    fi
    ip netns exec "$RCV" "$PY" "$BENCH/v2_client.py" --server "$SERVER_IP" --name receiver --create b1 \
      --video-fec-k "$fec_v" --audio-fec-k "$fec_a" $nack --end-at "$end_at" --out "$RUNDIR/$tag.recv.json" \
      >"$RUNDIR/$tag.recv.log" 2>&1 &
    recv_pid=$!
    sleep 1
    ip netns exec "$SND" "$PY" "$BENCH/v2_client.py" --server "$SERVER_IP" --name sender --join 1 \
      --send-video --send-audio --video-fec-k "$fec_v" --audio-fec-k "$fec_a" --end-at "$end_at" \
      --out "$RUNDIR/$tag.send.json" >"$RUNDIR/$tag.send.log" 2>&1 &
    send_pid=$!
  fi
  # Sample the receiver's IPv4 reassembly memory (it is per namespace) twice a second.
  ip netns exec "$RCV" "$PY" "$BENCH/lib/fragmem.py" "$end_at" >"$RUNDIR/$tag.frag.txt" &
  frag_pid=$!
  pids=("$send_pid" "$recv_pid" "$frag_pid")
  [ -z "$relay_pid" ] || pids+=("$relay_pid")
  wait_all "${pids[@]}" || status=$?
  if [ "$status" -eq 0 ]; then
    printf '{"run_id": "%s", "condition": "%s", "system": "%s", "loss_per_mille": %s, "repeat": %s, "seconds": %s, "nft": %s, "snmp_rcv": %s, "snmp_srv": %s, "ipfrag_high_thresh": %s, "ipfrag_time": %s}\n' \
      "$CS305_RUN_ID" "$cond" "$system" "$loss" "$rep" "$secs" "$(nft_counters)" "$(snmp "$RCV")" "$(snmp "$SRV")" \
      "$(ip netns exec "$RCV" cat /proc/sys/net/ipv4/ipfrag_high_thresh)" \
      "$(ip netns exec "$RCV" cat /proc/sys/net/ipv4/ipfrag_time)" >"$RUNDIR/$tag.net.json"
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
  local rep cond system loss s
  chunk_begin
  for rep in $(seq 1 "$REPEATS"); do
    for cond in $CONDITIONS; do
      [ "$cond" != downlink-long ] || continue
      local systems=$SYSTEMS losses=$LOSSES
      [ "$cond" != both-rtt50 ] || systems=$HARD_SYSTEMS losses=$HARD_LOSSES
      for loss in $losses; do
        for system in $systems; do
          s=0
          step "$cond-$system-$loss-r$rep" "$((SECONDS_PER_RUN + 8))" \
            run_one "$cond" "$system" "$loss" "$rep" "$SECONDS_PER_RUN" || s=$?
          case $s in 10 | 130) return "$s" ;; esac
        done
      done
    done
  done
  if [[ " $CONDITIONS " == *" downlink-long "* ]]; then
    for rep in $(seq 1 "$LONG_REPEATS"); do
      for loss in $LONG_LOSSES; do
        s=0
        step "downlink-long-legacy-$loss-r$rep" "$((LONG_SECONDS + 8))" \
          run_one downlink-long legacy "$loss" "$rep" "$LONG_SECONDS" || s=$?
        case $s in 10 | 130) return "$s" ;; esac
      done
    done
  fi
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
"$PY" "$BENCH/analyze_b1.py" "$RUNDIR" "$OUT" --build "$BUILD_DIR/build.json" --command "$(cat "$RUNDIR/command.txt")"
