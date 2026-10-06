#!/bin/bash
# The rooms the UI tests watch and join (docs/70 IX5, IX6), published by
# gawk-devpub to a local relay running with -rooms:
#
#   gawk-ios/scripts/room-fixtures.sh https://127.0.0.1:4499 smoke "$RUNNER_TEMP/rooms"
#
# Prints the tests' variables as NAME=value lines (for $GITHUB_ENV, or
# `export`), and leaves the publishers running, their logs in the out dir:
#
#   GAWK_UI_ROOM_CODE      five live streams, P1–P5 (Grid, Focus, People)
#   GAWK_UI_AWAY_ROOM      Keeper, live, and Mika, who drops off after 15 s
#                          (the away tile; the relay's grace must outlast
#                          the run)
#   GAWK_UI_CREATOR_ROOM   one stream, Guest, in a room minted by it; with
#   GAWK_UI_CREATOR_TOKEN  its creator token, a test joins as the creator
#                          and removes Guest
set -euo pipefail

relay="$1"
secret="$2"
out="$3"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
devpub="${GAWK_DEVPUB_BIN:-$here/../rust/target/debug/gawk-devpub}"
mkdir -p "$out"

# Starts one publisher in the background; its stdout goes to $out/<name>.out.
publish() {
  local name="$1"
  shift
  "$devpub" --url "$relay" --secret "$secret" --insecure --nick "$name" "$@" \
    > "$out/$name.out" 2> "$out/$name.log" &
}

# The value of KEY in a publisher's output, once it's printed (60 s at most).
wait_for() {
  local name="$1" key="$2"
  for _ in $(seq 120); do
    if grep -q "^$key=" "$out/$name.out" 2>/dev/null; then
      sed -n "s/^$key=//p" "$out/$name.out" | head -1
      return 0
    fi
    sleep 0.5
  done
  echo "error: $name never printed $key" >&2
  cat "$out/$name.log" >&2
  return 1
}

publish P1 --room-new
room="$(wait_for P1 GAWK_DEVPUB_ROOM)"
for n in P2 P3 P4 P5; do publish "$n" --room "$room"; done
for n in P2 P3 P4 P5; do wait_for "$n" GAWK_DEVPUB_ROOM > /dev/null; done

publish Keeper --room-new
away="$(wait_for Keeper GAWK_DEVPUB_ROOM)"
publish Mika --room "$away" --quit-after 15
wait_for Mika GAWK_DEVPUB_ROOM > /dev/null

publish Guest --room-new
creator_room="$(wait_for Guest GAWK_DEVPUB_ROOM)"
creator_token="$(wait_for Guest GAWK_DEVPUB_CREATOR)"

echo "GAWK_UI_ROOM_CODE=$room"
echo "GAWK_UI_AWAY_ROOM=$away"
echo "GAWK_UI_CREATOR_ROOM=$creator_room"
echo "GAWK_UI_CREATOR_TOKEN=$creator_token"
