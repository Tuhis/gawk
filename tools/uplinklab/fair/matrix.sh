#!/usr/bin/env bash
# The shared-bottleneck matrix (docs/75 RU4, §11 table O): each delivery mode
# and controller alone on the uplink and with a household's bulk TCP upload,
# plus the TCP flow alone, at 30 and 15 Mbps (1080p) and 80 Mbps (1440p),
# three runs per 1080p cell and two per 1440p cell. Needs Docker with
# NET_ADMIN, tools/uplinklab/build.sh and traces.sh first. Results land in
# $UPLINK_OUT/fair/<cell>-s<n>; a cell that finished is not re-run.
# FAIR_CCS picks the controllers (default "cubic bbr", the prototype sender;
# "engine-cubic engine-bbr" is the shipped engine, RU6) and FAIR_NONE=0 skips
# the TCP-alone baseline. Each cell records the host's CPU steal (% of CPU
# time) for 1 s before it (steal-pre.txt) and over it (steal.txt); a cell
# over FAIR_STEAL_MAX (default 5) is moved aside (.steal<n>) and run again,
# twice at most.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
JOBS=${JOBS:-3}
CCS=${FAIR_CCS:-cubic bbr}
STEAL_MAX=${FAIR_STEAL_MAX:-5}
# shellcheck source=../steal.sh
. "$HERE/../steal.sh"
docker build -q -t uplinklab-fair "$HERE" > /dev/null
mkdir -p "$OUT/fair"
attempt() { # name rate mode cc trace tcp
  local name=$1 nix=() p0 c0
  [ -d /nix ] && nix=(-v /nix:/nix:ro)   # a Nix-built sender needs its store
  p0=$(cpu); sleep 1; c0=$(cpu)
  timeout 300 docker run --rm --cap-add NET_ADMIN "${nix[@]}" -e OWNER="$(id -u):$(id -g)" \
    -v "$OUT:/lab" -v "$HERE/run.sh:/run.sh:ro" uplinklab-fair \
    sh /run.sh "/lab/fair/$name" "$2" "$3" "$4" "/lab/traces/$5" "$6" || return
  steal "$p0" "$c0" > "$OUT/fair/$name/steal-pre.txt"
  steal "$c0" "$(cpu)" > "$OUT/fair/$name/steal.txt"
  touch "$OUT/fair/$name/done"
}
one() { # name rate mode cc trace tcp
  local name=$1 n s
  [ -f "$OUT/fair/$name/done" ] && return
  for n in 1 2 3; do
    attempt "$@"
    [ -f "$OUT/fair/$name/done" ] || break
    s=$(cat "$OUT/fair/$name/steal.txt")
    awk -v s="$s" -v m="$STEAL_MAX" 'BEGIN { exit !(s > m) }' && [ $n -lt 3 ] || break
    echo "steal $s % on $name: re-running"
    mv "$OUT/fair/$name" "$OUT/fair/$name.steal$n"
  done
  if [ -f "$OUT/fair/$name/done" ]; then echo "done $name (steal $(cat "$OUT/fair/$name/steal.txt") %)"; else echo "FAILED $name"; fi
}
export -f attempt one cpu steal; export OUT HERE STEAL_MAX
cells() { # rate trace seeds
  for s in $(seq 1 "$3"); do
    [ "${FAIR_NONE:-1}" = 0 ] || echo "r$1-none-s$s ${1}mbit none cubic $2 1"
    for m in datagram carrier; do for c in $CCS; do
      echo "r$1-$m-$c-tcp-s$s ${1}mbit $m $c $2 1"
      echo "r$1-$m-$c-alone-s$s ${1}mbit $m $c $2 0"
    done; done
  done
}
run_cells() { # jobs
  while read -r line; do echo "$line"; done | xargs -P "$1" -L 1 bash -c 'one "$@"' _
}
{ cells 30 r1080p60.csv 3; cells 15 r1080p60.csv 3; } | run_cells "$JOBS"
cells 80 r1440p60.csv 2 | run_cells 1   # alone on the host, as §4 requires at 1440p
echo fair-finished
