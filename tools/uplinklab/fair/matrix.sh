#!/usr/bin/env bash
# The shared-bottleneck matrix (docs/75 RU4, §11 table O): each delivery mode
# and controller alone on the uplink and with a household's bulk TCP upload,
# plus the TCP flow alone, at 30 and 15 Mbps (1080p) and 80 Mbps (1440p),
# three runs per 1080p cell and two per 1440p cell. Needs Docker with
# NET_ADMIN, tools/uplinklab/build.sh and traces.sh first. Results land in
# $UPLINK_OUT/fair/<cell>-s<n>; a cell that finished is not re-run.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
JOBS=${JOBS:-3}
docker build -q -t uplinklab-fair "$HERE" > /dev/null
mkdir -p "$OUT/fair"
one() { # name rate mode cc trace tcp
  local name=$1 nix=(); [ -f "$OUT/fair/$name/done" ] && return
  [ -d /nix ] && nix=(-v /nix:/nix:ro)   # a Nix-built sender needs its store
  timeout 300 docker run --rm --cap-add NET_ADMIN "${nix[@]}" -e OWNER="$(id -u):$(id -g)" \
    -v "$OUT:/lab" -v "$HERE/run.sh:/run.sh:ro" uplinklab-fair \
    sh /run.sh "/lab/fair/$name" "$2" "$3" "$4" "/lab/traces/$5" "$6" && touch "$OUT/fair/$name/done"
  if [ -f "$OUT/fair/$name/done" ]; then echo "done $name"; else echo "FAILED $name"; fi
}
export -f one; export OUT HERE
cells() { # rate trace seeds
  for s in $(seq 1 "$3"); do
    echo "r$1-none-s$s ${1}mbit none cubic $2 1"
    for m in datagram carrier; do for c in cubic bbr; do
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
