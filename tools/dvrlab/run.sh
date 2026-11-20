#!/usr/bin/env bash
# run.sh <out-dir> <go|firefox> <K> <secs>: K deep-buffer sessions from one
# client to the running lab relay (relay-up.sh). go = gawk-loadgen, one QUIC
# connection per session; firefox = e2e/dvr-throughput-profile.mjs in the
# Playwright image. Writes sessions.csv (t_s,session,bytes), the relay's
# /statusz mid-run and its metrics before and after.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${DVRLAB_OUT:-$ROOT/tools/dvrlab/out}
# The image's browsers must be the ones e2e's playwright-core expects.
pw=$(sed -n '/"node_modules\/playwright-core": {/,/version/s/.*"version": "\(.*\)".*/\1/p' "$ROOT/e2e/package-lock.json")
PW_IMAGE=${PW_IMAGE:-mcr.microsoft.com/playwright:v$pw-noble}
dir=$1 client=$2 K=$3 secs=$4
mkdir -p "$dir"; dir=$(cd "$dir" && pwd)
id=$(docker exec dvrlab-relay sh -c "sed -n 's/^GAWK_PUBSIM_ID=//p' /tmp/pub.log")
metrics() { docker exec dvrlab-relay sh -c 'curl -s http://127.0.0.1:9100/metrics'; }
metrics > "$dir/metrics-before.txt"
case $client in
go)
  docker run --rm --network dvrlab -v "$OUT/bin:/b:ro" -v "$dir:/out" dvrlab-alpine \
    sh -c "/b/gawk-loadgen -url https://dvrlab-relay:4433 -insecure -id $id -viewers $K -ramp-ms 200 \
      -delivery deep -buffer-ms 3000 -duration ${secs}s -report 10s -session-csv /out/sessions.csv > /out/client.log 2>&1" &
  ;;
firefox)
  # A fresh copy of build.sh's profile, whose NSS store trusts the lab CA.
  docker run --rm --network dvrlab --ipc=host -v "$ROOT:$ROOT:ro" -v "$OUT:$OUT:ro" -v "$dir:$dir" \
    -e RELAY=https://dvrlab-relay:4433 -e ID="$id" -e K="$K" -e SECS="$secs" -e OUT="$dir/sessions.csv" \
    "$PW_IMAGE" bash -c "P=\$(mktemp -d) && cp -r $OUT/ff-profile/. \$P/ && cd $ROOT/e2e && \
      PROFILE=\$P node dvr-throughput-profile.mjs" > "$dir/client.log" 2>&1 &
  ;;
*) echo "client must be go or firefox" >&2; exit 2 ;;
esac
pid=$!
sleep $(( secs / 2 + 8 ))
docker exec dvrlab-relay sh -c 'curl -s http://127.0.0.1:9100/statusz' > "$dir/statusz.json"
wait $pid || true
metrics > "$dir/metrics.txt"
