#!/usr/bin/env bash
# app-run.sh <out-dir> "<netem args>" <auto|on|off> <secs>: the production web
# viewer (gawk-app/dist, Chromium) in Most stable with Striping seeded to the
# given value, against a fresh lab relay whose egress is shaped by netem
# (relay-up.sh). R87 SG4, docs/91 §8. Run build.sh first, and
# `npm run build` in gawk-app. Writes e2e/dvr-striped-app-profile.mjs's
# outputs (samples.csv, timeline.csv, summary.json, …) plus the relay's log.
#
#   tools/dvrlab/app-run.sh out/app/loss1-auto "delay 40ms loss 1% rate 50mbit limit 400" auto 300
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
H=$ROOT/tools/dvrlab
OUT=${DVRLAB_OUT:-$H/out}
pw=$(sed -n '/"node_modules\/playwright-core": {/,/version/s/.*"version": "\(.*\)".*/\1/p' "$ROOT/e2e/package-lock.json")
PW_IMAGE=${PW_IMAGE:-mcr.microsoft.com/playwright:v$pw-noble}
dir=$1 netem=$2 stripe=$3 secs=$4
mkdir -p "$dir"; dir=$(cd "$dir" && pwd)
[ -f "$ROOT/gawk-app/dist/index.html" ] || { echo "build gawk-app first (npm run build)" >&2; exit 2; }
# vite preview bundles its config into here; the tree is mounted read-only.
mkdir -p "$ROOT/gawk-app/node_modules/.vite-temp"
id=$("$H/relay-up.sh" "$netem")
echo "$netem" > "$dir/netem.txt"
name=dvrlab-app-$$
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; docker exec dvrlab-relay cat /tmp/relay.log > "$dir/relay.log" 2>/dev/null || true' EXIT
docker run --rm --name "$name" --network dvrlab --ipc=host --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$ROOT:$ROOT:ro" -v "$OUT/pki:$OUT/pki:ro" -v "$dir:$dir" \
  --tmpfs "$ROOT/gawk-app/node_modules/.vite-temp:mode=1777" \
  -e RELAY=https://dvrlab-relay:4433 -e OPS=http://dvrlab-relay:9100 -e ID="$id" \
  -e STRIPE="$stripe" -e SECS="$secs" -e CERT="$OUT/pki/relay.pem" -e OUT="$dir" \
  "$PW_IMAGE" bash -c "cd $ROOT/e2e && node dvr-striped-app-profile.mjs" 2>&1 | tee "$dir/client.log"
