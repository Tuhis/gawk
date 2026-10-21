#!/usr/bin/env bash
# The relay's datagram queue by depth (docs/75 F16, §11 table N): 1440p through
# the real relay on 3 pinned cores with 2 and 4 viewers (fan-out not
# saturated), three runs a cell; then 1080p with 15 viewers squeezed onto one
# core (overloaded), twice; then 1440p with 15 viewers on 3 cores and on 1
# (past capacity), once. Needs build.sh and traces.sh first; results land in
# $UPLINK_OUT/queue/<cell>-s<n>, and a finished run is not re-run.
# QUEUE_DEPTHS picks the depths (default all four) and QUEUE_PAST=0 skips the
# past-capacity cells: RU2's criterion is QUEUE_DEPTHS=512 QUEUE_PAST=0. Each
# run records the host's CPU steal over it (steal.txt, % of CPU time): the
# loss and latency figures mean nothing on a host whose hypervisor takes its
# cores.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${UPLINK_OUT:-$HERE/out}
P="-rtt 10ms -rate 300e6 -queue 50ms"
DEPTHS=${QUEUE_DEPTHS:-32 128 512 4096}
cpu() { head -1 /proc/stat; }
steal() { echo "$1|$2" | awk -F'|' '{split($1,a," "); split($2,b," "); t = 0
  for (i = 2; i <= 11; i++) t += b[i] - a[i]; printf "%.1f\n", 100 * (b[9] - a[9]) / t}'; }
run() { # name depth trace relaycpus viewers
  [ -f "$OUT/queue/$1/metrics.txt" ] && return
  local c0; c0=$(cpu)
  RELAY_BIN=$OUT/bin/gawk-server-q$2 RELAY_CPUS=$4 VIEWER_CPUS=3-4 SENDER_CPUS=5-6 SUB=1 PROXY_ARGS="$P" \
    timeout 200 "$HERE/relay.sh" "$OUT/queue/$1" "$OUT/traces/$3" "$5" 60 > /dev/null 2>&1
  # relay.sh leaves metrics.txt even when the sender died early: a run with
  # an empty log on either side is discarded, not averaged into table N.
  steal "$c0" "$(cpu)" > "$OUT/queue/$1/steal.txt"
  if [ -s "$OUT/queue/$1/send.csv" ] && [ -s "$OUT/queue/$1/recv.csv" ]; then
    echo "done $1 (steal $(cat "$OUT/queue/$1/steal.txt") %)"
  else
    mv "$OUT/queue/$1" "$OUT/queue/$1.failed.$$"
    echo "FAILED $1"
  fi
}
for s in 1 2 3; do for v in 2 4; do for q in $DEPTHS; do
  run "v$v-q$q-s$s" $q r1440p60.csv 0-2 $v
done; done; done
for s in 1 2; do for q in $DEPTHS; do run "o1080-q$q-s$s" $q r1080p60.csv 0 15; done; done
# Past capacity, once each: the fan-out saturates 3 cores at 1440p, and 1 core.
[ "${QUEUE_PAST:-1}" = 0 ] && exit 0
for q in $DEPTHS; do run "v15-q$q-s1" $q r1440p60.csv 0-2 15; run "o1440-q$q-s1" $q r1440p60.csv 0 15; done
