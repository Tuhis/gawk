#!/usr/bin/env bash
# The relay's datagram queue by depth (docs/73 F16, §11 table N), and the
# stock depth drained on its own goroutine (drain.patch): 1440p through
# the real relay on 3 pinned cores with 2 and 4 viewers (fan-out not
# saturated), three runs a cell; then 1080p with 15 viewers squeezed onto one
# core (overloaded), twice; then 1440p with 15 viewers on 3 cores and on 1
# (past capacity), once. Needs build.sh and traces.sh first; results land in
# $UPLINK_OUT/queue/<cell>-s<n>, and a finished run is not re-run.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${UPLINK_OUT:-$HERE/out}
P="-rtt 10ms -rate 300e6 -queue 50ms"
run() { # name depth trace relaycpus viewers
  [ -f "$OUT/queue/$1/metrics.txt" ] && return
  local bin=gawk-server
  case $2 in 32) ;; drain) bin=gawk-server-drain ;; *) bin=gawk-server-q$2 ;; esac
  RELAY_BIN=$OUT/bin/$bin RELAY_CPUS=$4 VIEWER_CPUS=3-4 SENDER_CPUS=5-6 SUB=1 PROXY_ARGS="$P" \
    timeout 200 "$HERE/relay.sh" "$OUT/queue/$1" "$OUT/traces/$3" "$5" 60 > /dev/null 2>&1
  # relay.sh leaves metrics.txt even when the sender died early: a run with
  # an empty log on either side is discarded, not averaged into table N.
  if [ -s "$OUT/queue/$1/send.csv" ] && [ -s "$OUT/queue/$1/recv.csv" ]; then
    echo "done $1"
  else
    mv "$OUT/queue/$1" "$OUT/queue/$1.failed.$$"
    echo "FAILED $1"
  fi
}
for s in 1 2 3; do for v in 2 4; do for q in 32 128 512 4096 drain; do
  c=q$q; [ $q = drain ] && c=drain
  run "v$v-$c-s$s" $q r1440p60.csv 0-2 $v
done; done; done
for s in 1 2; do for q in 32 128 512 4096; do run "o1080-q$q-s$s" $q r1080p60.csv 0 15; done; done
# Past capacity, once each: the fan-out saturates 3 cores at 1440p, and 1 core.
for q in 32 128 512 4096; do run "v15-q$q-s1" $q r1440p60.csv 0-2 15; run "o1440-q$q-s1" $q r1440p60.csv 0 15; done
