#!/usr/bin/env bash
# One lab run: receiver, proxy and sender on loopback.
#   run.sh <out-dir> <sender args...> -- <proxy args...>
# RECV_ARGS adds receiver flags (e.g. -stream-window); RECV_BIN swaps the
# receiver binary (e.g. gawk-uplinklab-bigq, D7's longer datagram queue; the
# default is gawk-uplinklab-q32, the stock depth §11 measured as today's);
# SENDER_BIN the sender (browser/sender.mjs: Chrome, docs/75 OD5), which
# finds the receiver's certificate hash in <out-dir>/cert.hash.
set -u
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
LAB=$OUT/bin/gawk-uplinklab
SND=${SENDER_BIN:-$OUT/bin/sender}
dir=$1; shift
s_args=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do s_args+=("$1"); shift; done
[ $# -gt 0 ] && shift
p_args=("$@")
mkdir -p "$dir"
port=$(( 20000 + RANDOM % 20000 ))
pport=$(( port + 1 ))
# shellcheck disable=SC2086
"${RECV_BIN:-$LAB-q32}" recv -addr 127.0.0.1:$port -out "$dir/recv.csv" -cert-hash-file "$dir/cert.hash" ${RECV_ARGS:-} \
  2> "$dir/recv.log" &
rp=$!
$LAB proxy -listen 127.0.0.1:$pport -upstream 127.0.0.1:$port -stats "$dir/proxy.txt" "${p_args[@]}" 2> "$dir/proxy.log" &
pp=$!
sleep 0.3
$SND --url https://127.0.0.1:$pport/lab --out "$dir/send.csv" "${s_args[@]}" 2> "$dir/send.log"
sleep 0.5
kill -INT $pp 2>/dev/null; kill -INT $rp 2>/dev/null
wait $pp $rp 2>/dev/null
