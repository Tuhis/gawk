#!/usr/bin/env bash
# The shared uplink with the browser sender (docs/73 OD5, §11 table O): run.sh
# in a rootless network namespace (`unshare -rn`) on the host, because the
# sender is the host's Chrome. Each mode beside the TCP upload twice and alone
# once, at 30 and 15 Mbps (1080p) and 80 Mbps (1440p). Needs build.sh (it
# builds the browser sender's page), traces.sh, Chrome for Testing and
# iperf3 (IPERF3). Results land beside matrix.sh's in $UPLINK_OUT/fair.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
IPERF3=${IPERF3:-$(command -v iperf3)}
[ -x "$IPERF3" ] || { echo "fair/chrome.sh: no iperf3 (set IPERF3)" >&2; exit 1; }
mkdir -p "$OUT/fair"
one() { # name rate mode trace tcp
  [ -f "$OUT/fair/$1/done" ] && return
  timeout 300 unshare -rn env LAB="$OUT" UPLINK_OUT="$OUT" IPERF3="$IPERF3" \
    SENDER="node $ROOT/tools/uplinklab/browser/sender.mjs" \
    sh "$HERE/run.sh" "$OUT/fair/$1" "$2" "$3" chrome "$OUT/traces/$4" "$5" && touch "$OUT/fair/$1/done"
  if [ -f "$OUT/fair/$1/done" ]; then echo "done $1"; else echo "FAILED $1"; fi
}
for r in 30:1080 15:1080 80:1440; do
  rate=${r%:*} tr=r${r#*:}p60.csv
  for m in datagram carrier; do
    for s in 1 2; do one "r$rate-$m-chrome-tcp-s$s" "${rate}mbit" $m "$tr" 1; done
    one "r$rate-$m-chrome-alone-s1" "${rate}mbit" $m "$tr" 0
  done
done
echo chrome-fair-finished
