#!/usr/bin/env bash
# R74 PP9 (docs/76 §11.3): a PoP's downlink leg beside a game update. run.sh
# in a rootless network namespace (`unshare -rn`, as fair/chrome.sh), with the
# shaped direction the venue's downlink: the leg's media (home's leg writer,
# carrier or datagram) and a download of TCP_FLOWS parallel flows (iperf3 -R
# -P, kernel Cubic, default 4) through one tbf with a 50 ms drop-tail queue,
# acknowledgements back unshaped. The sender is `gawk-uplinklab legsend` (the
# hub's SubscribeLeg writer, the same one /pop/subscribe runs at home, with
# LegOptions.Carrier set by ?delivery=carrier); the receiver gets the PoP leg
# dialer's receive windows at gawk-relay's default -uplink-max-bitrate 50mbps
# (uplinkReceiveWindows: 2000000 / 3000000). Cells: 250 and 100 Mbps with the
# 1080p60 trace, 100 Mbps with the 1440p60 trace (the stream a large share of
# the link), and 100 Mbps 1080p beside 8 flows; three runs a cell, each mode
# beside the download and alone, plus the download alone. One run at a time.
# Before each run it waits (up to DOWN_LOAD_WAIT s, default 900) for the
# host's 1-minute load average to fall under DOWN_LOAD_MAX (default 4), then
# records it (load.txt) and the host's CPU steal for 1 s before the run
# (steal-pre.txt) and over it (steal.txt). Needs build.sh's bin/, traces.sh
# and iperf3 (IPERF3). Results in $UPLINK_OUT/down; fair/analyze.py reads
# them with FAIR_SET=down. A run that finished is not re-run.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
IPERF3=${IPERF3:-$(command -v iperf3)}
[ -x "$IPERF3" ] || { echo "fair/downlink.sh: no iperf3 (set IPERF3)" >&2; exit 1; }
# shellcheck source=../steal.sh
. "$HERE/../steal.sh"
RES=$OUT/down
mkdir -p "$RES"
load1() { cut -d' ' -f1 /proc/loadavg; }
one() { # name rate mode trace tcp flows
  local d=$RES/$1 p0 c0 waited=0
  [ -f "$d/done" ] && return
  rm -rf "$d"
  while awk -v l="$(load1)" -v m="${DOWN_LOAD_MAX:-4}" 'BEGIN { exit !(l > m) }' && [ $waited -lt "${DOWN_LOAD_WAIT:-900}" ]; do
    sleep 10; waited=$((waited + 10))
  done
  mkdir -p "$d"
  echo "$(load1) waited=${waited}s" > "$d/load.txt"
  p0=$(cpu); sleep 1; c0=$(cpu)
  local mode=$3 cc=pop
  [ "$mode" = none ] && cc=cubic
  timeout 300 unshare -rn env LAB="$OUT" UPLINK_OUT="$OUT" IPERF3="$IPERF3" TCP_REVERSE=1 TCP_FLOWS="$6" \
    RECV_ARGS="-stream-window 2000000 -conn-window 3000000" \
    sh "$HERE/run.sh" "$d" "$2" "$mode" "$cc" "$OUT/traces/$4" "$5" || { echo "FAILED $1"; return; }
  steal "$p0" "$c0" > "$d/steal-pre.txt"
  steal "$c0" "$(cpu)" > "$d/steal.txt"
  touch "$d/done"
  echo "done $1 (load $(cat "$d/load.txt"), steal $(cat "$d/steal.txt") %)"
}
cells() { # key rate trace flows
  for s in 1 2 3; do
    one "r$1-none-s$s" "$2" none "$3" 1 "$4"
    for m in datagram carrier; do
      one "r$1-$m-pop-tcp-s$s" "$2" $m "$3" 1 "$4"
      [ "$4" = 4 ] && one "r$1-$m-pop-alone-s$s" "$2" $m "$3" 0 "$4"
    done
  done
}
cells 250 250mbit r1080p60.csv 4
cells 100 100mbit r1080p60.csv 4
cells 100x1440 100mbit r1440p60.csv 4
cells 100p8 100mbit r1080p60.csv 8
echo downlink-finished
