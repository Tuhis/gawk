#!/usr/bin/env bash
# R74 PP8 (docs/76 §11.3): an outside viewer's time to first frame for a
# party broadcast, at a netem RTT on the PoP <-> home path.
#
#   pop-ttff.sh <out-dir> <pop-rtt-ms> <viewer-rtt-ms> <carrier|datagram> [joins]
#
# Everything runs in one rootless network namespace (`unshare -rn`, as
# fair/chrome.sh): a home gawk-server (-pops, a -pops-file naming one PoP by a
# static credential, -pop-uplink-linger 2s), a gawk-relay linked to it with
# that credential (`-pair gawk-cred:...`, as e2e/pop-*.mjs do) and
# -pop-leg-delivery <carrier|datagram>, and one gawk-pubsim publishing on the
# PoP (30 fps, keyframes padded to 230000 bytes, docs/75's 1080p60 trace's
# mean keyframe, and deltas to 15000). Home listens on every address; the PoP
# reaches it as 127.0.0.2 and the viewer as 127.0.0.3, and a prio qdisc on lo
# puts netem (half the RTT each way) on each path by those addresses, so the
# two paths are delayed independently. tc's per-class counters (qdisc.txt)
# show both directions of each path went through their netem.
#
# Each join is cold: it waits until home has handed the broadcast back to a
# reservation (the linger's "no demand" line), so there is no demand, no leg
# and no hub at home before it, and the PoP dials a fresh leg. The viewer is
# `gawk-uplinklab ttff` (dial -> session -> config -> complete keyframe ->
# first delta, ms from just before the dial); it holds the session 4 s, and
# 1 s after its first frame a second viewer joins the live leg (warm). One
# JSON line per viewer lands in <out-dir>/ttff.jsonl; pop-ttff.py tabulates.
# Needs build.sh's bin/ (gawk-server, gawk-relay, gawk-pubsim, gawk-devcert,
# gawk-uplinklab). Before the run it waits (up to TTFF_LOAD_WAIT s, default
# 900) for the host's 1-minute load average to fall under TTFF_LOAD_MAX
# (default 4) and records it (load.txt), and the CPU steal over the run
# (steal.txt, steal.sh).
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
B=$OUT/bin
if [ "${1:-}" != --in-netns ]; then
  # shellcheck source=steal.sh
  . "$HERE/steal.sh"
  load1() { cut -d' ' -f1 /proc/loadavg; }
  waited=0
  while awk -v l="$(load1)" -v m="${TTFF_LOAD_MAX:-4}" 'BEGIN { exit !(l > m) }' && [ $waited -lt "${TTFF_LOAD_WAIT:-900}" ]; do
    sleep 10; waited=$((waited + 10))
  done
  mkdir -p "$1"
  l0="$(load1) waited=${waited}s"
  c0=$(cpu)
  timeout 900 unshare -rn bash "$0" --in-netns "$@" || exit
  echo "$l0" > "$1/load.txt"
  steal "$c0" "$(cpu)" > "$1/steal.txt"
  exit 0
fi
shift
out=$1 poprtt=$2 viewrtt=$3 delivery=$4 joins=${5:-12}
mkdir -p "$out"
rm -rf "$out/ttff.jsonl" "$out/ttff.err" "$out/pop-state"  # a stale credential would be refused
ip link set lo up
tc qdisc add dev lo root handle 1: prio bands 5 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
tc qdisc add dev lo parent 1:1 handle 10: pfifo limit 100000
q=2
for spec in "127.0.0.2 dst $poprtt" "127.0.0.2 src $poprtt" "127.0.0.3 dst $viewrtt" "127.0.0.3 src $viewrtt"; do
  set -- $spec
  half=$(awk -v r="$3" 'BEGIN { printf "%gms", r / 2 }')
  tc qdisc add dev lo parent 1:$q handle $((q * 10)): netem delay "$half" limit 100000
  tc filter add dev lo parent 1: protocol ip prio 1 u32 match ip "$2" "$1/32" flowid 1:$q
  q=$((q + 1))
done
pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT
"$B/gawk-devcert" -out "$out/cert" -hosts localhost,127.0.0.1,127.0.0.2,127.0.0.3 > /dev/null
cred=lab-$RANDOM$RANDOM$RANDOM
sha=$(printf %s "$cred" | sha256sum | cut -d' ' -f1)
printf '[{"name":"lab-party","uid":"00000000-0000-4000-8000-000000000001","displayName":"Lab party","credentialSHA256":"%s"}]\n' \
  "$sha" > "$out/pops.json"
"$B/gawk-server" -addr 0.0.0.0:4433 -metrics-addr 127.0.0.1:9100 -cert-file "$out/cert/cert.pem" \
  -key-file "$out/cert/key.pem" -pops -pops-file "$out/pops.json" -pop-uplink-linger 2s > "$out/home.log" 2>&1 &
pids+=($!)
until curl -sf http://127.0.0.1:9100/healthz > /dev/null; do sleep 0.1; done
SSL_CERT_FILE=$out/cert/cert.pem "$B/gawk-relay" -addr 127.0.0.1:4443 -state-dir "$out/pop-state" \
  -console-addr 127.0.0.1:0 -lan-address 127.0.0.1 -pop-leg-delivery "$delivery" \
  -pair "gawk-cred:127.0.0.2:4433/$cred" > "$out/pop.log" 2>&1 &
pids+=($!)
for _ in $(seq 300); do grep -q 'linked to home' "$out/pop.log" && break; sleep 0.1; done
grep -q 'linked to home' "$out/pop.log" || { echo "pop-ttff: the PoP never linked" >&2; exit 1; }
"$B/gawk-pubsim" -url https://127.0.0.1:4443 -insecure -fps 30 -pad-keyframe-bytes 230000 -pad-delta-bytes 15000 \
  -stats 0 > "$out/pubsim.log" 2>&1 &
pids+=($!)
for _ in $(seq 200); do grep -q GAWK_PUBSIM_ID= "$out/pubsim.log" && break; sleep 0.1; done
id=$(sed -n 's/^GAWK_PUBSIM_ID=//p' "$out/pubsim.log" | head -1)
[ -n "$id" ] || { echo "pop-ttff: pubsim printed no ID" >&2; exit 1; }
sleep 2
handed() { grep -c 'reservation handed back' "$out/home.log"; }
for i in $(seq "$joins"); do
  if [ "$i" -gt 1 ]; then
    for _ in $(seq 900); do [ "$(handed)" -ge $((i - 1)) ] && break; sleep 0.1; done
    [ "$(handed)" -ge $((i - 1)) ] || { echo "pop-ttff: join $i: the leg never lingered out" >&2; exit 1; }
    sleep 1
  fi
  "$B/gawk-uplinklab" ttff -url https://127.0.0.3:4433 -id "$id" -hold 4s -label "cold-$i" > "$out/cold.tmp" 2>> "$out/ttff.err" &
  cp=$!
  for _ in $(seq 200); do [ -s "$out/cold.tmp" ] && break; sleep 0.05; done
  sleep 1
  "$B/gawk-uplinklab" ttff -url https://127.0.0.3:4433 -id "$id" -label "warm-$i" >> "$out/ttff.jsonl" 2>> "$out/ttff.err"
  wait $cp
  cat "$out/cold.tmp" >> "$out/ttff.jsonl"
  rm -f "$out/cold.tmp"
done
tc -s class show dev lo > "$out/qdisc.txt"
tc -s qdisc show dev lo >> "$out/qdisc.txt"
echo "pop-ttff: $joins joins of $id in $out"
