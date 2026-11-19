#!/usr/bin/env bash
# B1 loss resilience: frame delivery and audio loss at 0/1/2/5/10 % random packet loss.
#
#   sender ns ──veth (MTU 1500)── server ns ──veth (MTU 1500)── receiver ns
#                                                     nftables prerouting, priority -500:
#                                                     drop N per mille of UDP packets
#
# The loss rule runs before IPv4 defragmentation (priority -400), so it drops single
# IP fragments, as a lossy link does. Only the server -> receiver direction is
# impaired. Systems: the original code (legacy relay + legacy client media threads),
# v2 without FEC, v2 with FEC (video k=4, audio k=2). P2P is disabled on the v2 server
# so both systems relay through the server (legacy: relay mode, as `chgmode` selects).
#
# Usage: sudo bench/run_b1.sh            (all systems, all loss rates)
#        SYSTEMS="v2-fec" LOSSES="50" SECONDS_PER_RUN=10 bench/run_b1.sh
# Writes bench/results/b1_loss.json. Each system's sweep holds the shared bench lock.
set -euo pipefail
source "$(dirname "$0")/lib/common.sh"

SYSTEMS=${SYSTEMS:-"legacy v2-nofec v2-fec"}
LOSSES=${LOSSES:-"0 10 20 50 100"} # per mille
SECONDS_PER_RUN=${SECONDS_PER_RUN:-41}
OUT_DIR="$TMPDIR_BENCH/b1"
SRV=${NS_PREFIX}1srv SND=${NS_PREFIX}1snd RCV=${NS_PREFIX}1rcv
SERVER_IP=10.77.0.1 SND_IP=10.77.1.2 RCV_IP=10.77.2.2

setup_ns() {
  local loss=$1
  cleanup_netns
  ip netns add "$SRV"; ip netns add "$SND"; ip netns add "$RCV"
  for ns in "$SRV" "$SND" "$RCV"; do ip -n "$ns" link set lo up; done
  ip -n "$SRV" addr add "$SERVER_IP/32" dev lo
  ip link add b1s0 netns "$SND" type veth peer name b1s1 netns "$SRV"
  ip link add b1r0 netns "$RCV" type veth peer name b1r1 netns "$SRV"
  ip -n "$SND" link set b1s0 mtu 1500 up; ip -n "$SRV" link set b1s1 mtu 1500 up
  ip -n "$RCV" link set b1r0 mtu 1500 up; ip -n "$SRV" link set b1r1 mtu 1500 up
  # The v2 server sends forwarded video as UDP GSO messages, which a real NIC (or the
  # kernel, just before the driver) splits into datagrams before they reach the wire.
  # veth would carry them whole into the receiver namespace, where the loss rule would
  # then drop a whole message at a time; one segment per packet on the server's side
  # makes the kernel split them as it transmits, so the rule sees single datagrams.
  ip -n "$SRV" link set b1r1 gso_max_segs 1
  ip -n "$SND" addr add "$SND_IP/24" dev b1s0; ip -n "$SRV" addr add 10.77.1.1/24 dev b1s1
  ip -n "$RCV" addr add "$RCV_IP/24" dev b1r0; ip -n "$SRV" addr add 10.77.2.1/24 dev b1r1
  ip -n "$SND" route add 10.77.0.0/24 via 10.77.1.1
  ip -n "$RCV" route add 10.77.0.0/24 via 10.77.2.1
  ip netns exec "$RCV" nft -f - <<EOF
table ip cs305loss {
  counter udp_in {}
  counter udp_drop {}
  chain pre {
    type filter hook prerouting priority -500; policy accept;
    ip protocol udp counter name udp_in
    ip protocol udp numgen random mod 1000 < $loss counter name udp_drop drop
  }
}
EOF
}

nft_counters() {
  ip netns exec "$RCV" nft -j list counters | "$PY" -c '
import json, sys
data = json.load(sys.stdin)["nftables"]
print(json.dumps({c["counter"]["name"]: c["counter"]["packets"] for c in data if "counter" in c}))'
}

run_one() {
  local system=$1 loss=$2 tag="$1-$2" end_at server_pid
  setup_ns "$loss"
  log "B1 $system loss=${loss}/1000"
  if [ "$system" = legacy ]; then
    ip netns exec "$SRV" "$PY" "$BENCH/legacy_harness/server.py" --ip "$SERVER_IP" >"$OUT_DIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$SRV" "$SERVER_IP" 8888
    end_at=$(mono_plus "$SECONDS_PER_RUN")
    ip netns exec "$RCV" "$PY" "$BENCH/legacy_harness/client.py" --id 2 --ip "$RCV_IP" --server-ip "$SERVER_IP" \
      --create --end-at "$end_at" --out "$OUT_DIR/$tag.recv.json" >"$OUT_DIR/$tag.recv.log" 2>&1 &
    local recv_pid=$!
    sleep 1
    ip netns exec "$SND" "$PY" "$BENCH/legacy_harness/client.py" --id 1 --ip "$SND_IP" --server-ip "$SERVER_IP" \
      --send --end-at "$end_at" --out "$OUT_DIR/$tag.send.json" >"$OUT_DIR/$tag.send.log" 2>&1 &
    local send_pid=$!
  else
    local fec_v=4 fec_a=2
    if [ "$system" = v2-nofec ]; then fec_v=0 fec_a=0; fi
    ip netns exec "$SRV" "$CONFSERVER" -control-addr "$SERVER_IP:6100" -media-addr "$SERVER_IP:6100" \
      -metrics-addr 127.0.0.1:6101 -p2p=false -audio-fec-k "$fec_a" >"$OUT_DIR/$tag.server.log" 2>&1 &
    server_pid=$!
    wait_tcp "$SRV" "$SERVER_IP" 6100
    end_at=$(mono_plus "$SECONDS_PER_RUN")
    ip netns exec "$RCV" "$PY" "$BENCH/v2_client.py" --server "$SERVER_IP" --name receiver --create b1 \
      --video-fec-k "$fec_v" --audio-fec-k "$fec_a" --end-at "$end_at" --out "$OUT_DIR/$tag.recv.json" \
      >"$OUT_DIR/$tag.recv.log" 2>&1 &
    local recv_pid=$!
    sleep 1
    ip netns exec "$SND" "$PY" "$BENCH/v2_client.py" --server "$SERVER_IP" --name sender --join 1 \
      --send-video --send-audio --video-fec-k "$fec_v" --audio-fec-k "$fec_a" --end-at "$end_at" \
      --out "$OUT_DIR/$tag.send.json" >"$OUT_DIR/$tag.send.log" 2>&1 &
    local send_pid=$!
  fi
  # Sample the receiver's IPv4 reassembly memory (it is per namespace) twice a second.
  ip netns exec "$RCV" "$PY" "$BENCH/lib/fragmem.py" "$end_at" >"$OUT_DIR/$tag.frag.txt" &
  local frag_pid=$!
  wait "$send_pid" "$recv_pid" "$frag_pid"
  printf '{"system": "%s", "loss_per_mille": %s, "nft": %s, "snmp_rcv": %s, "snmp_srv": %s, "ipfrag_high_thresh": %s, "ipfrag_time": %s}\n' \
    "$system" "$loss" "$(nft_counters)" "$(snmp "$RCV")" "$(snmp "$SRV")" \
    "$(ip netns exec "$RCV" cat /proc/sys/net/ipv4/ipfrag_high_thresh)" \
    "$(ip netns exec "$RCV" cat /proc/sys/net/ipv4/ipfrag_time)" >"$OUT_DIR/$tag.net.json"
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  cleanup_netns
}

if [ "${1:-}" = --sweep ]; then
  # Inner invocation: one system, all loss rates, under the bench lock.
  trap cleanup_netns EXIT INT TERM
  for loss in $LOSSES; do run_one "$2" "$loss"; done
  exit 0
fi

require_root
prepare
mkdir -p "$OUT_DIR"
trap cleanup_netns EXIT INT TERM
for system in $SYSTEMS; do
  log "waiting for the bench lock ($LOCK) for $system"
  flock "$LOCK" "$0" --sweep "$system"
done
"$PY" "$BENCH/analyze_b1.py" "$OUT_DIR" "$RESULTS/b1_loss.json"
