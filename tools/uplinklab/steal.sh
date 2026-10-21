#!/usr/bin/env bash
# The host's CPU steal, % of CPU time (docs/75 §11): the loss and latency
# figures mean nothing on a host whose hypervisor takes its cores. Sourced by
# queue.sh and matrix.py's engine runs for cpu/steal; run directly, it is the
# pre-flight check: 5 s idle, then 15 s under STEAL_LOAD (default 4) busy
# loops, printing both. Above ~5 % under load the matrix belongs to a night run.
cpu() { head -1 /proc/stat; }
steal() { echo "$1|$2" | awk -F'|' '{split($1,a," "); split($2,b," "); t = 0
  for (i = 2; i <= 11; i++) t += b[i] - a[i]; printf "%.1f\n", t ? 100 * (b[9] - a[9]) / t : 0}'; }
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  a=$(cpu); sleep 5; b=$(cpu); echo "idle $(steal "$a" "$b")"
  pids=()
  for _ in $(seq "${STEAL_LOAD:-4}"); do (while :; do :; done) & pids+=($!); done
  a=$(cpu); sleep 15; b=$(cpu); kill "${pids[@]}"
  echo "load $(steal "$a" "$b")"
fi
