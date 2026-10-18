#!/usr/bin/env bash
# F7's runs (docs/73): a publish to the REAL relay with gawk-loadgen viewers
# attached, reading the relay's own ingress-loss counters afterwards.
#   relay.sh <out-dir> <trace.csv> <viewers> <seconds> [sender args...]
# RELAY_CPUS, VIEWER_CPUS and SENDER_CPUS pin each side to its own cores
# (taskset), so the viewers' CPU does not count against the relay.
# RELAY_BIN overrides the relay binary; PROXY_ARGS, when set, puts the lab's
# proxy (with those arguments) between the sender and the relay; SUB=1 adds a
# lab viewer whose log gives per-frame latency through the relay (analyze.py).
set -u
pin() { local cpus=$1; shift; if [ -n "$cpus" ]; then exec taskset -c "$cpus" "$@"; else exec "$@"; fi; }
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
B=$OUT/bin
out=$1 trace=$2 viewers=$3 secs=$4; shift 4
mkdir -p "$out"
[ -f "$OUT/cert/cert.pem" ] || "$B/gawk-devcert" -out "$OUT/cert" >/dev/null
port=$(( 20000 + RANDOM % 20000 )); mport=$(( port + 1 )); pport=$(( port + 2 ))
pin "${RELAY_CPUS:-}" "${RELAY_BIN:-$B/gawk-server}" -addr 127.0.0.1:$port -metrics-addr 127.0.0.1:$mport \
  -cert-file "$OUT/cert/cert.pem" -key-file "$OUT/cert/key.pem" > "$out/relay.log" 2>&1 &
rp=$!
until curl -sf http://127.0.0.1:$mport/healthz >/dev/null; do sleep 0.1; done
target=$port
pp=
if [ -n "${PROXY_ARGS:-}" ]; then
  # shellcheck disable=SC2086
  pin "${SENDER_CPUS:-}" "$B/gawk-uplinklab" proxy -listen 127.0.0.1:$pport -upstream 127.0.0.1:$port \
    -stats "$out/proxy.txt" $PROXY_ARGS 2> "$out/proxy.log" &
  pp=$!
  target=$pport
  sleep 0.3
fi
rm -f "$out/id.txt"
pin "${SENDER_CPUS:-}" "$B/sender" --url https://127.0.0.1:$target/publish --id-file "$out/id.txt" --trace "$trace" \
  --seconds "$secs" --out "$out/send.csv" "$@" 2> "$out/send.log" &
sp=$!
until [ -s "$out/id.txt" ]; do sleep 0.1; done
if [ "$viewers" -gt 0 ]; then
  pin "${VIEWER_CPUS:-}" "$B/gawk-loadgen" -url https://127.0.0.1:$port -id "$(cat "$out/id.txt")" -viewers "$viewers" \
    -duration "$(( secs - 2 ))s" -insecure > "$out/loadgen.log" 2>&1 &
fi
subp=
if [ -n "${SUB:-}" ]; then
  # A lab viewer on the longer quic-go queue, so its own queue never drops:
  # per-frame latency sender -> relay -> viewer lands in recv.csv.
  pin "${VIEWER_CPUS:-}" "$B/gawk-uplinklab-bigq" sub -url https://127.0.0.1:$port -id-file "$out/id.txt" \
    -out "$out/recv.csv" 2> "$out/sub.log" &
  subp=$!
fi
wait $sp
sleep 0.5
[ -n "$subp" ] && kill -INT $subp
curl -s http://127.0.0.1:$mport/metrics > "$out/metrics.txt"
[ -n "$pp" ] && kill -INT $pp
grep -E "VmHWM|VmRSS" /proc/$rp/status > "$out/relay-mem.txt" 2>/dev/null
kill $rp; wait $rp 2>/dev/null
wait
grep -E "^gawk_relay_(ingress|datagrams_relayed|frames_relayed)" "$out/metrics.txt"
