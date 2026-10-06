#!/bin/bash
# Whether every fixture broadcast still delivers video: one gawk-loadgen
# viewer per broadcast for a few seconds, which must see keyframe streams
# and delta frames. A fixture that stopped delivering fails here rather than
# as a decode count in an app test (the first CI run to reach the tests,
# 2026-10-06, had a room stream deliver its cached keyframe and nothing
# after it).
#
#   gawk-ios/scripts/fixtures-streaming.sh https://127.0.0.1:4499 path/to/gawk-loadgen ID...
set -euo pipefail

relay="$1"
loadgen="$2"
shift 2
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

for id in "$@"; do
  "$loadgen" -url "$relay" -id "$id" -viewers 1 -duration 4s -report 4s -insecure \
    > "$out/$id.txt" 2>&1 &
done
wait

failed=0
for id in "$@"; do
  deltas="$(sed -n 's/^delta frames: *//p' "$out/$id.txt" | tail -1)"
  keyframes="$(sed -n 's/^keyframe streams: *//p' "$out/$id.txt" | tail -1)"
  echo "$id: ${keyframes:-?} keyframe streams, ${deltas:-?} delta frames in 4 s"
  if [ "${deltas:-0}" -eq 0 ] || [ "${keyframes:-0}" -eq 0 ]; then
    echo "error: $id isn't streaming" >&2
    cat "$out/$id.txt" >&2
    failed=1
  fi
done
exit "$failed"
