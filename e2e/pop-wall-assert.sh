#!/usr/bin/env bash
# R74 PP11 (docs/76 §6.10, §11) — the party wall with a REAL PoP: a
# gawk-relay on the runner, paired with the kind fleet through a Pop CR,
# publishing party broadcasts that two relay pods put on a wall Room CR.
#
# rooms-assert.sh step 3b proves the home half (a spec.pop room refuses an
# ordinary attach). internal/popsrv's wall_test.go runs the rows below
# in-process against one home pod; what only this tier adds is the chart's
# Pop CRD and RBAC (the pairing writes status.credential, the link writes
# status.link), a link and uplink legs that land on whichever pod the load
# balancer picks while the wall is homed on its own lease, and the
# gawk-relay binary's own startup.
#
# What it proves, in order:
#
#   1. Pairing on the real chart: a pairing string whose hash is on a Pop CR
#      redeems, the PoP says "linked to home", and the CR's status carries
#      the credential and a connected link.
#   2. Every party broadcast is on the wall, a FIFTH and SIXTH included
#      (the fleet's -max-room-broadcasts default is 4; the wall's own
#      spec.maxBroadcasts wins): an outside participant at home sees each
#      tile added, and a fresh joiner's RoomState lists all six, labelled
#      with the broadcaster's name or "Stream n". The named broadcaster's
#      own room session at the PoP is the wall's state from home (link-
#      proxied room control: it sees the outsider), and its 0x18 says
#      linked, on the wall, with the wall code.
#   3. Hide removes the tile and Show restores it (the broadcaster's Detach
#      and Attach at the PoP, forwarded home vouched), and the broadcaster's
#      0x18 follows each: on-wall clear after Hide, set after Show, the
#      wall code kept throughout.
#   4. A broadcaster that remembered Hide (wall=hidden on /publish) never
#      appears: its 0x18 says off the wall, and the outside participant
#      never saw its tile added, not even for a moment.
#   5. The uplink budget (spec.uplink.maxStreams: 1): an outside viewer of
#      one party tile gets frames; an outside viewer of a second is closed
#      with 4012, the code the app draws as "Party-only right now"; once
#      the first viewer leaves and the leg lingers out, a retry of the
#      second joins and gets frames — the tile's 20 s retry finding the
#      freed slot.
#   6. Unlinked (the Pop revoked), party guests see the local wall: a
#      guest's room at the PoP is read-only and lists the PoP's own live,
#      unhidden broadcasts with their labels, and a broadcaster's 0x18 says
#      unlinked with the wall code kept.
#
# The copy itself ("Party-only right now" for a 4012 tile) is pinned by
# gawk-app's RoomScreen.test.tsx, and that Chrome reads 4012 in-band by
# e2e/pop-uplink-full.mjs (ci.yml's rooms-pop group); this tier pins the
# wire behind it on the real fleet. An ordinary Attach to the wall being
# refused is rooms-assert.sh step 3b.
#
# Usage: pop-wall-assert.sh <namespace> [deadline-seconds]
#   GAWK_E2E_URL      home (the kind NodePort), default https://127.0.0.1:4433
#   GAWK_E2E_POP_PORT the PoP's UDP port on loopback, default 4434
#   GAWK_E2E_HOME_CA  home's certificate (PEM), default e2e/out/certs/cert.pem
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$E2E_DIR/bin"
OUT="$E2E_DIR/out/pop-wall"
rm -rf "$OUT"
mkdir -p "$OUT"
HOME_URL=${GAWK_E2E_URL:-https://127.0.0.1:4433}
HOME_EP=${HOME_URL#https://}
POP_PORT=${GAWK_E2E_POP_PORT:-4434}
POP_URL="https://127.0.0.1:$POP_PORT"
HOME_CA=${GAWK_E2E_HOME_CA:-$E2E_DIR/out/certs/cert.pem}

NS=${1:?usage: pop-wall-assert.sh <namespace> [deadline]}
DEADLINE=${2:-90}

POP=e2e-party
WALL=E2EParty
WALL_CR=e2eparty
GROUP_API="/apis/gawk.ioio.fi/v1alpha1/namespaces/$NS"

PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  exec 3>&- 2>/dev/null || true
  # The fixtures go, so the tiers after this one see the fleet as they
  # expect it. Raw paths, for the discovery reason rooms-assert.sh gives.
  kubectl delete --raw "$GROUP_API/rooms/$WALL_CR" >/dev/null 2>&1 || true
  kubectl delete --raw "$GROUP_API/pops/$POP" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- PoP log (tail):" >&2
  tail -n 40 "$OUT/pop.log" >&2 2>/dev/null || true
  echo "--- Pop CR status:" >&2
  kubectl get --raw "$GROUP_API/pops/$POP" 2>&1 | jq -c '.status' >&2 || true
  for f in "$OUT"/*.out; do
    [ -e "$f" ] || continue
    echo "--- $(basename "$f") (tail):" >&2
    tail -n 5 "$f" >&2
  done
  for p in $(kubectl -n "$NS" get pods -l app.kubernetes.io/name=gawk-server -o name 2>/dev/null); do
    echo "--- $p wall/pop log lines:" >&2
    kubectl -n "$NS" logs "$p" --tail=-1 2>&1 | grep -iE 'pop|wall|room|error' | tail -n 30 >&2 || true
  done
  exit 1
}

# wait_for <what> <command...> — retry until the command succeeds.
wait_for() {
  local what=$1 end=$((SECONDS + DEADLINE))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -ge "$end" ] && fail "$what (not within ${DEADLINE}s)"
    sleep 0.5
  done
}

# home_tiles — the wall's attachments as a fresh outside joiner at home
# sees them in its first RoomState: "ID=label" pairs, one per line.
home_tiles() {
  local log="$OUT/home-joiner.out"
  "$BIN/gawk-roomsim" -url "$HOME_URL" -insecure -code "$WALL" -nick peek -duration 2s > "$log" 2>/dev/null || true
  grep -m1 '"type":"state"' "$log" | jq -r '.attachments[] | "\(.broadcastID)=\(.label)"'
}
has_tile() { home_tiles | grep -q "^$1="; }
lacks_tile() { ! has_tile "$1"; }

# last_info <roomsim stdout> — the broadcaster's latest 0x18, as JSON.
last_info() { grep '"type":"localRelay"' "$1" | tail -n 1; }
info_is() { last_info "$1" | jq -e "$2" >/dev/null; }

# pub_id <stdout> <regex> — wait for the broadcast ID line; sets ID.
# (Never called in $(...): the broadcasters are started in this shell so
# PIDS holds them for cleanup, and a subshell's PIDS would be lost.)
pub_id() {
  local log=$1 end=$((SECONDS + 30))
  ID=""
  while true; do
    ID=$(grep -m1 -oE "$2" "$log" 2>/dev/null | grep -oE '[A-Z0-9]{6}' | tail -n 1 || true)
    [ -n "$ID" ] && return 0
    [ "$SECONDS" -ge "$end" ] && fail "no broadcast ID in $(basename "$log") ($(tail -n 3 "$log.err" 2>/dev/null))"
    sleep 0.2
  done
}

# roomsim_pub <name> [extra args...] — a media-less broadcaster at the PoP
# named <name>, publishing until killed; sets ID.
roomsim_pub() {
  local name=$1
  shift
  "$BIN/gawk-roomsim" -url "$POP_URL" -insecure -publish -nick "$name" -duration 0 "$@" \
    > "$OUT/pub-$name.out" 2> "$OUT/pub-$name.out.err" &
  PIDS+=($!)
  pub_id "$OUT/pub-$name.out" '"broadcastID":"[A-Z0-9]{6}"'
}

# pubsim_pub <n> — a fixture-publishing broadcaster at the PoP (unnamed,
# so its tile reads "Stream n"); sets ID.
pubsim_pub() {
  "$BIN/gawk-pubsim" -url "$POP_URL" -insecure -duration 600s > "$OUT/pubsim-$1.out" 2> "$OUT/pubsim-$1.out.err" &
  PIDS+=($!)
  pub_id "$OUT/pubsim-$1.out" 'GAWK_PUBSIM_ID=[A-Z0-9]{6}'
}

# ------------------------------------------------------- 1. pair the PoP
TOKEN=$(head -c 16 /dev/urandom | base32 | tr -d '=')
TOKEN_SHA=$(printf '%s' "$TOKEN" | sha256sum | cut -d' ' -f1)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
later=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)
# What gawk-admin's wizard writes (docs/76 item 33): the Pop with its wall
# code, and the wall Room with spec.pop. The uplink budget is ONE stream so
# step 5's second outside tile is the one past it.
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: gawk.ioio.fi/v1alpha1
kind: Pop
metadata:
  name: $POP
spec:
  displayName: "E2E party"
  pairing: {tokenSHA256: "$TOKEN_SHA", issuedAt: "$now", expiresAt: "$later"}
  uplink: {maxStreams: 1}
  limits: {maxBroadcasts: 40}
  wall: {roomCode: $WALL}
---
apiVersion: gawk.ioio.fi/v1alpha1
kind: Room
metadata:
  name: $WALL_CR
spec:
  kind: static
  displayCode: $WALL
  displayName: "E2E party wall"
  pop: $POP
  maxBroadcasts: 40
EOF

# The PoP's dialer verifies home against the system roots: SSL_CERT_FILE
# makes the throwaway devcert one. Every listener is pinned to loopback or
# an ephemeral port, so nothing collides with the forwards the other
# assert scripts hold.
SSL_CERT_FILE="$HOME_CA" "$BIN/gawk-relay" -addr "127.0.0.1:$POP_PORT" -state-dir "$OUT/pop-state" \
  -console-addr 127.0.0.1:0 -ui-addr 127.0.0.1:0 -metrics-addr 127.0.0.1:0 -lan-address 127.0.0.1 \
  -pair "gawk-pair:$HOME_EP/$TOKEN" > "$OUT/pop.log" 2>&1 &
PIDS+=($!)
wait_for "the PoP never linked to home" grep -q 'linked to home' "$OUT/pop.log"
pop_linked() {
  kubectl get --raw "$GROUP_API/pops/$POP" | jq -e --arg t "$TOKEN_SHA" \
    '.status.credential.sha256 != null and .status.credential.pairingTokenSHA256 == $t and .status.link.connected == true and (.status.lease.holder // "") != ""'
}
wait_for "the Pop CR's status never showed the credential and a connected link" pop_linked
LINK_POD=$(kubectl get --raw "$GROUP_API/pops/$POP" | jq -r '.status.lease.holder')
echo "PASS: paired through the Pop CR — the PoP linked, status.credential and status.link written by $LINK_POD"

# ------------------------------------------- 2. every party broadcast, 6 of them
OUTSIDER="$OUT/outsider.out"
"$BIN/gawk-roomsim" -url "$HOME_URL" -insecure -code "$WALL" -nick outsider -duration 0 \
  > "$OUTSIDER" 2> "$OUTSIDER.err" &
PIDS+=($!)
wait_for "the outside participant never got the wall's RoomState" grep -q '"type":"state"' "$OUTSIDER"
[ $(( $(grep -m1 '"type":"state"' "$OUTSIDER" | jq -r .flags) & 4 )) -eq 0 ] \
  || fail "the wall granted an attach to an outside participant"

# The named broadcaster holds a room session on the wall at the PoP and
# takes Hide/Show on stdin (fd 3).
mkfifo "$OUT/tuhis.cmd"
"$BIN/gawk-roomsim" -url "$POP_URL" -insecure -publish -code "$WALL" -nick tuhis -duration 0 \
  < "$OUT/tuhis.cmd" > "$OUT/pub-tuhis.out" 2> "$OUT/pub-tuhis.out.err" &
PIDS+=($!)
exec 3> "$OUT/tuhis.cmd"
pub_id "$OUT/pub-tuhis.out" '"broadcastID":"[A-Z0-9]{6}"'
B1=$ID
# Two media-carrying tiles for step 5, then three more media-less ones.
pubsim_pub 2
B2=$ID
pubsim_pub 3
B3=$ID
roomsim_pub p4
B4=$ID
roomsim_pub p5
B5=$ID
roomsim_pub p6
B6=$ID
ALL="$B1 $B2 $B3 $B4 $B5 $B6"
echo "party broadcasts at the PoP: $ALL"

for b in $ALL; do
  wait_for "the outside participant never saw $b added to the wall" \
    jq -e --arg b "$b" 'select(.type == "event" and .kind == 16 and .attachment.broadcastID == $b)' "$OUTSIDER"
done
tiles=$(home_tiles)
for b in $ALL; do grep -q "^$b=" <<<"$tiles" || fail "a fresh joiner's wall lacks $b (tiles: $(tr '\n' ' ' <<<"$tiles"))"; done
[ "$(wc -l <<<"$tiles")" -eq 6 ] || fail "the wall has $(wc -l <<<"$tiles") tiles, want 6: $(tr '\n' ' ' <<<"$tiles")"
grep -q "^$B1=tuhis$" <<<"$tiles" || fail "the named broadcaster's tile is not labelled tuhis: $(grep "^$B1=" <<<"$tiles")"
grep -qE "^$B2=Stream [0-9]+$" <<<"$tiles" || fail "an unnamed broadcaster's tile is not 'Stream n': $(grep "^$B2=" <<<"$tiles")"
echo "PASS: all 6 party broadcasts are on the wall at home, the fifth and sixth included (labels: $(tr '\n' ' ' <<<"$tiles"))"

wait_for "the broadcaster's room session at the PoP never got the wall's state" grep -q '"type":"state"' "$OUT/pub-tuhis.out"
grep -m1 '"type":"state"' "$OUT/pub-tuhis.out" | jq -e --arg w "$WALL" \
  '.code == $w and ([.participants[].nickname] | index("outsider") != null)' >/dev/null \
  || fail "the broadcaster's wall session at the PoP is not home's wall: $(grep -m1 '"type":"state"' "$OUT/pub-tuhis.out")"
wait_for "the broadcaster's 0x18 never said linked and on the wall $WALL ($(last_info "$OUT/pub-tuhis.out"))" \
  info_is "$OUT/pub-tuhis.out" ".linked and .onWall and .wallCode == \"$WALL\""
echo "PASS: the broadcaster's wall session at the PoP is proxied home (it sees the outsider), and its 0x18 says linked, on wall $WALL"

# ---------------------------------------------------------- 3. Hide, Show
echo hide >&3
wait_for "Hide did not take $B1 off the wall" lacks_tile "$B1"
# The outsider's event comes over its own session, maybe from another
# pod, so it can land after a fresh joiner's RoomState already lacks B1.
saw_removed() {
  jq -e --arg b "$B1" 'select(.type == "event" and .kind == 17 and .attachment.broadcastID == $b)' "$OUTSIDER" >/dev/null
}
wait_for "the outside participant saw no AttachmentRemoved for $B1 after Hide" saw_removed
wait_for "the broadcaster's 0x18 never cleared on-wall after Hide ($(last_info "$OUT/pub-tuhis.out"))" \
  info_is "$OUT/pub-tuhis.out" ".linked and (.onWall | not) and .wallCode == \"$WALL\""
echo "PASS: Hide took $B1 off the wall (AttachmentRemoved at home) and its 0x18 cleared on-wall, wall code kept"
echo show >&3
wait_for "Show did not put $B1 back on the wall" has_tile "$B1"
wait_for "the broadcaster's 0x18 never set on-wall after Show ($(last_info "$OUT/pub-tuhis.out"))" \
  info_is "$OUT/pub-tuhis.out" ".linked and .onWall and .wallCode == \"$WALL\""
echo "PASS: Show put $B1 back on the wall and its 0x18 set on-wall again"

# --------------------------------------------------------- 4. wall=hidden
roomsim_pub shy -wall-hidden
B7=$ID
wait_for "the hidden broadcaster's 0x18 never arrived linked with the wall code" \
  info_is "$OUT/pub-shy.out" ".linked and .wallCode == \"$WALL\""
# Long enough for a vouched attach to have crossed (step 2's took well
# under a second), then the whole history on both sides is the evidence.
sleep 3
grep '"type":"localRelay"' "$OUT/pub-shy.out" | jq -e 'select(.onWall)' >/dev/null \
  && fail "a wall=hidden broadcaster's 0x18 said on the wall"
jq -e --arg b "$B7" 'select(.type == "event" and .kind == 16 and .attachment.broadcastID == $b)' "$OUTSIDER" >/dev/null \
  && fail "the outside participant saw the wall=hidden broadcaster $B7 added"
lacks_tile "$B7" || fail "the wall=hidden broadcaster $B7 is on the wall"
echo "PASS: the wall=hidden broadcaster $B7 never appeared (no AttachmentAdded at home, 0x18 off the wall)"

# ---------------------------------------------------- 5. the uplink budget
# Each tile is an ordinary /subscribe at home (docs/76 item 35).
LG=("$BIN/gawk-loadgen" -url "$HOME_URL" -insecure -viewers 1 -report 1s)
"${LG[@]}" -id "$B2" -duration 600s > "$OUT/viewer-first.out" 2>&1 &
FIRST=$!
PIDS+=($FIRST)
wait_for "the first outside viewer of $B2 never got frames" grep -qE 'kf/s=[1-9]' "$OUT/viewer-first.out"
"${LG[@]}" -id "$B3" -duration 15s -expect-close-code 4012 > "$OUT/viewer-past-budget.out" 2>&1 \
  || fail "the outside viewer past the budget was not closed with 4012: $(grep -E 'FAIL|session' "$OUT/viewer-past-budget.out" | tr '\n' ' ')"
echo "PASS: one stream forwarded ($B2 has frames at home); the second tile's viewer ($B3) was closed with 4012 (\"Party-only right now\")"
kill "$FIRST" 2>/dev/null || true
end=$((SECONDS + DEADLINE))
attempt=0
while true; do
  attempt=$((attempt + 1))
  "${LG[@]}" -id "$B3" -duration 6s > "$OUT/viewer-retry.out" 2>&1 || true
  grep -qE '^keyframe streams: +[1-9]' "$OUT/viewer-retry.out" && break
  [ "$SECONDS" -ge "$end" ] && fail "after the first viewer left, no retry of $B3 got frames within ${DEADLINE}s ($attempt attempts)"
  sleep 1
done
echo "PASS: once the first viewer left and its leg lingered out, retry $attempt of $B3 joined and got frames"

# ------------------------------------------------- 6. unlinked: the local wall
# Revoked is the operator's way to cut a link for good; the PoP keeps
# serving the LAN as a standalone relay (docs/76 §6.9). A raw PUT of the
# read object, for rooms-assert.sh's discovery reason. The link-home pod
# writes status.lease every 5 s (poplink.LeaseRenew), which bumps the
# resourceVersion the PUT carries, so a 409 here is a lost race: re-read.
revoke() {
  kubectl get --raw "$GROUP_API/pops/$POP" | jq '.spec.revoked = true' \
    | kubectl replace --raw "$GROUP_API/pops/$POP" -f - >/dev/null
}
revoked=0
for _ in 1 2 3 4 5; do
  if revoke 2> "$OUT/revoke.err"; then revoked=1; break; fi
  sleep 0.5
done
[ "$revoked" = 1 ] || fail "could not revoke the Pop: $(cat "$OUT/revoke.err")"
wait_for "the revoked PoP's broadcaster 0x18 never said unlinked ($(last_info "$OUT/pub-p4.out"))" \
  info_is "$OUT/pub-p4.out" "(.linked | not) and .wallCode == \"$WALL\""
GUEST="$OUT/guest.out"
local_wall() {
  "$BIN/gawk-roomsim" -url "$POP_URL" -insecure -code "$WALL" -nick guest -duration 2s > "$GUEST" 2> "$GUEST.err" || true
  grep -m1 '"type":"state"' "$GUEST" | jq -e --arg ids "$B2 $B3 $B4 $B5 $B6" \
    '([.attachments[].broadcastID] | sort) as $have | ($ids | split(" ") | sort) as $want | ($want - $have) == []'
}
wait_for "a party guest's local wall never listed the PoP's broadcasts ($(grep -m1 '"type":"state"' "$GUEST" 2>/dev/null))" local_wall
st=$(grep -m1 '"type":"state"' "$GUEST")
[ $(( $(jq -r .flags <<<"$st") & 4 )) -eq 0 ] || fail "the local wall is not read-only: $st"
jq -e --arg b "$B7" '[.attachments[].broadcastID] | index($b) == null' <<<"$st" >/dev/null \
  || fail "the local wall shows the wall=hidden broadcaster $B7"
jq -e --arg b "$B4" '.attachments[] | select(.broadcastID == $b) | .label == "p4"' <<<"$st" >/dev/null \
  || fail "the local wall's tile for $B4 is not labelled p4: $st"
echo "PASS: unlinked (revoked), a party guest sees the local read-only wall: $(jq -c '[.attachments[] | "\(.broadcastID)=\(.label)"]' <<<"$st"), and the 0x18 says unlinked with wall $WALL"

echo "PASS: R74 PP11 party wall end to end — paired PoP, six tiles, Hide/Show, wall=hidden, the 4012 budget and its freed slot, the local wall"
