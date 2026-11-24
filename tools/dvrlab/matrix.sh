#!/usr/bin/env bash
# docs/91 §11's runs. Each path is netem on the relay's egress; "delay" is the
# whole RTT, because the ACK path is not delayed.
set -u
H=$(cd "$(dirname "$0")" && pwd)
OUT=${DVRLAB_OUT:-$H/out}
path() { "$H/relay-up.sh" "$1" >/dev/null; }
run() { "$H/run.sh" "$OUT/runs/$1-$2-k$3" "$2" "$3" "$4"; echo "done $1-$2-k$3"; }
path "delay 40ms"; for c in go firefox; do run clean $c 1 90; done
for loss in 0.2 0.5 1; do
  path "delay 40ms loss ${loss}% rate 50mbit limit 400"
  for K in 1 2 3 4; do run "loss$loss" go $K 90; done
  for K in 1 2; do run "loss$loss" firefox $K 90; done
done
path "delay 40ms rate 8mbit limit 100"; for K in 1 2 3 4; do run link8 go $K 90; done
for rtt in 75 150; do path "delay ${rtt}ms rate 100mbit limit 3000"; for c in go firefox; do run "rtt$rtt" $c 1 60; done; done
docker rm -f dvrlab-relay >/dev/null
