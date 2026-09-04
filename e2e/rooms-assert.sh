#!/usr/bin/env bash
# R42 RM3 (docs/44 §4.5, §9) — rooms across a real two-pod relay fleet.
#
# The unit suite proves the lease and the proxy pipe against a fake API
# server and two in-process pods (internal/roomcluster, internal/transport).
# What only this tier can add is the real control plane: a Room CR applied
# with kubectl reaching every pod's informer through a real API server, the
# relay's RBAC actually permitting the status writes, the home-pod lease on a
# CR the chart's CRD validated, and a pod that dies for real rather than one
# whose context was cancelled.
#
# What it proves, in order:
#
#   1. the API serves rooms.gawk.ioio.fi (raw discovery) — the CRD rooms.enabled renders is real.
#   2. A STATIC room applied by kubectl is joinable through the load balancer
#      with no home assigned in advance: the pod that receives the first join
#      claims it (status.lease.holder names a relay pod), writes status.key,
#      and that key is the one the home's /statusz publishes with role "home".
#      A second joiner lands wherever conntrack sends it; the home's roster
#      still reaches 2, which is the proxy path when the second dial landed on
#      the other pod and the local path when it did not — either way the
#      room is joinable from both pods (§9's "joinable on both pods").
#   3. Deleting the static CR ends the room fleet-wide: the joiner's control
#      session closes with 4007, read off the session by gawk-roomsim.
#   4. A DYNAMIC room minted by gawk-pubsim exists as a CR whose
#      status.attachments names the broadcast and whose lease names the
#      minting pod.
#   5. The home pod is killed for real (`--grace-period=0 --force`: no
#      drain, no lease release — the lease goes stale on its own clock). A
#      re-dialled joiner lands in the room with the attachment list whole
#      within the deadline: the CR's lease moves to a surviving pod at a
#      higher generation, and the new home's /statusz carries the room.
#
# NOT asserted here, and why: that a browser participant's own reconnect
# gets it back in (that is RM4's E2E), and the janitor (its windows are
# minutes long by design; the store's unit test drives its clock).
#
# Usage: rooms-assert.sh <namespace> [deadline-seconds]
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBSIM="$E2E_DIR/bin/gawk-pubsim"
ROOMSIM="$E2E_DIR/bin/gawk-roomsim"
OUT="$E2E_DIR/out"
mkdir -p "$OUT"
RELAY_URL=${GAWK_E2E_URL:-https://127.0.0.1:4433}

NS=${1:?usage: rooms-assert.sh <namespace> [deadline]}
DEADLINE=${2:-120}

list_pods() {
  kubectl -n "$NS" get pods -l app.kubernetes.io/name=gawk-server \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}
mapfile -t PODS < <(list_pods)
if [ "${#PODS[@]}" -ne 2 ]; then
  echo "expected 2 relay pods in $NS, found ${#PODS[@]}: ${PODS[*]-}" >&2
  exit 1
fi

# The supervised port-forward pattern of cluster-assert.sh / moderation-
# assert.sh, for the same reasons — and doubly so here, where step 5 kills a
# pod out from under its forward on purpose.
declare -A PORT
forward() { # forward <pod> <local-port>
  local pod=$1 port=$2 kid=
  trap 'kill "$kid" 2>/dev/null; exit 0' TERM INT
  while true; do
    kubectl -n "$NS" port-forward "pod/$pod" "$port:2112" >/dev/null 2>&1 &
    kid=$!
    wait "$kid" 2>/dev/null || true
    sleep 1
  done
}
i=0
for p in "${PODS[@]}"; do
  PORT[$p]=$((21600 + i))
  forward "$p" "${PORT[$p]}" &
  i=$((i + 1))
done
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

CURL=(curl -sS --max-time 5 --retry 3 --retry-delay 1 --retry-connrefused)
statusz() { "${CURL[@]}" -f "http://127.0.0.1:${PORT[$1]}/statusz"; }

FORWARD_WAIT=60
for p in "${PODS[@]}"; do
  end=$((SECONDS + FORWARD_WAIT))
  until curl -fs --max-time 3 -o /dev/null "http://127.0.0.1:${PORT[$p]}/healthz"; do
    if [ "$SECONDS" -ge "$end" ]; then
      echo "port-forward to $p (127.0.0.1:${PORT[$p]} -> 2112) never answered /healthz within ${FORWARD_WAIT}s" >&2
      exit 1
    fi
    sleep 1
  done
done
echo "PASS: ops port-forwards ready on all ${#PODS[@]} pods (/healthz answered)"

fail() {
  echo "FAIL: $*" >&2
  for p in "${PODS[@]}"; do
    echo "--- $p /statusz rooms:" >&2
    statusz "$p" 2>/dev/null | jq '.rooms' >&2 || echo "(unreachable)" >&2
  done
  kubectl get --raw "$ROOMS_API" 2>&1 | jq . >&2 || true
  # Two kind runs in a row lost kubectl's discovery of `rooms` right after
  # a CR write while the API kept serving it; this is the evidence the
  # next occurrence needs — what the server says the group holds, what
  # kubectl's discovery cache says, and the CRD's own status.
  echo "--- discovery: /apis/gawk.ioio.fi/v1alpha1 resources:" >&2
  kubectl get --raw /apis/gawk.ioio.fi/v1alpha1 2>&1 | jq -c '[.resources[].name]' >&2 || true
  echo "--- kubectl api-resources --api-group=gawk.ioio.fi:" >&2
  kubectl api-resources --api-group=gawk.ioio.fi 2>&1 >&2 || true
  echo "--- crd rooms.gawk.ioio.fi status:" >&2
  kubectl get crd rooms.gawk.ioio.fi -o jsonpath='{.status}' 2>&1 >&2 || true
  echo >&2
  ls "$OUT"/roomsim-*.out >/dev/null 2>&1 && tail -n 5 "$OUT"/roomsim-*.out >&2
  # The relay's side of the story: the room lines from every pod, so a claim
  # that never landed or an RBAC refusal is visible here and not only in the
  # exported kind logs.
  for p in "${PODS[@]}"; do
    echo "--- $p room log lines:" >&2
    kubectl -n "$NS" logs "$p" --tail=-1 2>/dev/null | grep -iE 'room|forbidden|lease' | tail -n 40 >&2 || true
  done
  exit 1
}

# Rooms are read and deleted through RAW API paths, never through
# `kubectl get rooms`: two kind runs in a row had kubectl report "the
# server doesn't have a resource type rooms" for the rest of the step —
# even with the group-qualified name — seconds after `kubectl apply` had
# created a Room and while the API served the group fine (`bans` in the
# same group resolved at job end). Whatever kubectl's discovery does
# there, the assert is about the relay, and the raw path asks the API
# server directly; fail() dumps the discovery state for the next look.
ROOMS_API="/apis/gawk.ioio.fi/v1alpha1/namespaces/$NS/rooms"

# room_field <name> <jq path> — one field off the CR (jq syntax, e.g.
# '.status.lease.holder'). Empty when the CR exists and the field is unset;
# an HTTP 404 (the CR is gone) is also empty; any other kubectl failure is
# retried three times (an API blip) and then FAILS LOUDLY — it must never
# read as "status not written yet" (PR #302 review).
room_field() {
  local err out
  for _ in 1 2 3; do
    if out=$(kubectl get --raw "$ROOMS_API/$1" 2>"$OUT/kubectl.err"); then
      jq -r "$2 // empty" <<<"$out" | tr '\n' ' ' | sed 's/ *$//'
      return 0
    fi
    err=$(cat "$OUT/kubectl.err")
    grep -q "NotFound\|404" <<<"$err" && return 0
    sleep 1
  done
  fail "GET $ROOMS_API/$1 failed: $err"
}

# first_state <roomsim stdout> — the first RoomState line, or "".
first_state() { grep -m1 '"type":"state"' "$1" 2>/dev/null || true; }

# ---------------------------------------------------------------- 1. the CRD
kubectl get --raw /apis/gawk.ioio.fi/v1alpha1 | jq -e '.resources[] | select(.name=="rooms")' >/dev/null \
  || fail "the Room CRD is not installed — rooms.enabled did not render it"
echo "PASS: the API serves gawk.ioio.fi/v1alpha1 rooms (the CRD is installed)"

# ------------------------------------------------ 2. a static room, by kubectl
STATIC=e2eroom
kubectl -n "$NS" apply -f - <<EOF
apiVersion: gawk.ioio.fi/v1alpha1
kind: Room
metadata:
  name: $STATIC
spec:
  kind: static
  displayCode: E2ERoom
  displayName: "e2e static room"
EOF

# Hold the first join for the whole static section (step 3 needs a live
# session to see the 4007). Informer propagation is asynchronous: a join that
# lands before a pod's cache has the CR is a 404, so retry the dial.
STATIC_LOG="$OUT/roomsim-static-first.out"
end=$((SECONDS + DEADLINE))
FIRST_PID=""
while true; do
  "$ROOMSIM" -url "$RELAY_URL" -insecure -code E2ERoom -nick first -duration "$((DEADLINE + 60))s" \
    > "$STATIC_LOG" 2> "$STATIC_LOG.err" &
  FIRST_PID=$!
  for _ in $(seq 50); do
    [ -n "$(first_state "$STATIC_LOG")" ] && break
    kill -0 "$FIRST_PID" 2>/dev/null || break
    sleep 0.2
  done
  [ -n "$(first_state "$STATIC_LOG")" ] && break
  wait "$FIRST_PID" 2>/dev/null || true
  [ "$SECONDS" -ge "$end" ] && fail "no join of the static room $STATIC produced a RoomState within ${DEADLINE}s ($(cat "$STATIC_LOG.err"))"
  sleep 2
done
STATE=$(first_state "$STATIC_LOG")
[ "$(jq -r '.code' <<<"$STATE")" = "E2ERoom" ] || fail "static RoomState code = $(jq -r .code <<<"$STATE"), want the configured display code E2ERoom"
STATIC_KEY=$(jq -r '.key' <<<"$STATE")
[ -n "$STATIC_KEY" ] && [ "$STATIC_KEY" != "null" ] || fail "static RoomState carries no key"

# The claim: status.lease.holder names a pod, status.key equals the key the
# participant saw, and that pod's /statusz publishes the room under it as
# "home". The status writes are the RBAC's rooms/status grant in action.
end=$((SECONDS + DEADLINE))
HOME=""
while true; do
  HOME=$(room_field "$STATIC" '.status.lease.holder')
  crkey=$(room_field "$STATIC" '.status.key')
  if [ -n "$HOME" ] && [ "$crkey" = "$STATIC_KEY" ]; then break; fi
  [ "$SECONDS" -ge "$end" ] && fail "static room CR status not written within ${DEADLINE}s (holder='$HOME' key='$crkey', want key $STATIC_KEY)"
  sleep 1
done
[ -n "${PORT[$HOME]-}" ] || fail "static room lease holder '$HOME' is not one of the relay pods (${PODS[*]})"
statusz "$HOME" | jq -e --arg k "$STATIC_KEY" '.rooms[$k] | .role == "home" and .participants >= 1 and .kind == "static"' >/dev/null \
  || fail "home pod $HOME does not publish $STATIC_KEY as a static home room on /statusz"
echo "PASS: static room — first join claimed it on $HOME, status.key=$STATIC_KEY matches RoomState and /statusz (role home)"

# A second joiner, wherever conntrack lands it. The home's roster reaching 2
# is the whole claim: from the other pod that is one proxied control stream.
SECOND_LOG="$OUT/roomsim-static-second.out"
"$ROOMSIM" -url "$RELAY_URL" -insecure -code e2eroom -nick second -duration 20s > "$SECOND_LOG" 2>&1 &
SECOND_PID=$!
end=$((SECONDS + 30))
until statusz "$HOME" | jq -e --arg k "$STATIC_KEY" '.rooms[$k].participants >= 2' >/dev/null 2>&1; do
  [ "$SECONDS" -ge "$end" ] && fail "the home pod never saw the second joiner ($(cat "$SECOND_LOG"))"
  sleep 1
done
other=""
for p in "${PODS[@]}"; do [ "$p" != "$HOME" ] && other=$p; done
if statusz "$other" | jq -e --arg k "$STATIC_KEY" '.rooms[$k].role == "proxy"' >/dev/null 2>&1; then
  echo "PASS: second joiner landed on $other and was PROXIED to $HOME (role proxy on $other, roster 2 on the home)"
else
  echo "PASS: second joiner landed on $HOME directly (roster 2 on the home); the proxy row is covered by the transport suite"
fi
wait "$SECOND_PID" 2>/dev/null || true

# ------------------------------------- 3. deleting the CR ends it: 4007
kubectl delete --raw "$ROOMS_API/$STATIC" >/dev/null
end=$((SECONDS + DEADLINE))
until grep -q '"type":"close","code":4007' "$STATIC_LOG" 2>/dev/null; do
  [ "$SECONDS" -ge "$end" ] && fail "the static room's participant was not closed with 4007 after the CR was deleted ($(tail -n 3 "$STATIC_LOG"))"
  sleep 1
done
grep -q '"kind":32' "$STATIC_LOG" || fail "no RoomEnding event preceded the 4007"
wait "$FIRST_PID" 2>/dev/null || true
echo "PASS: deleting the static CR sent RoomEnding then close 4007 to its participant"

# -------------------------------------- 4. a dynamic room minted by pubsim
"$PUBSIM" -url "$RELAY_URL" -insecure -duration "$((3 * DEADLINE + 120))s" -room-new -label pc -nick pubsim \
  > "$OUT/pubsim-room.out" 2> "$OUT/pubsim-room.err" &
PUBSIM_PID=$!
for _ in $(seq 150); do
  grep -q '^ROOM ' "$OUT/pubsim-room.out" && break
  sleep 0.2
done
grep -q '^ROOM ' "$OUT/pubsim-room.out" || fail "gawk-pubsim -room-new minted no room: $(tail -n 5 "$OUT/pubsim-room.err")"
BID=$(grep -m1 -o 'GAWK_PUBSIM_ID=[A-Z0-9]*' "$OUT/pubsim-room.out" | cut -d= -f2)
CODE=$(grep -m1 '^ROOM ' "$OUT/pubsim-room.out" | awk '{print $2}')
DYN=$(printf '%s' "$CODE" | tr '[:upper:]' '[:lower:]')

end=$((SECONDS + DEADLINE))
while true; do
  kind=$(room_field "$DYN" '.spec.kind')
  holder=$(room_field "$DYN" '.status.lease.holder')
  att=$(room_field "$DYN" '[.status.attachments[]?.broadcastID] | join(" ")')
  if [ "$kind" = dynamic ] && [ -n "$holder" ] && [[ " $att " == *" $BID "* ]]; then break; fi
  [ "$SECONDS" -ge "$end" ] && fail "dynamic room CR $DYN not as minted within ${DEADLINE}s (kind='$kind' holder='$holder' attachments='$att', want $BID)"
  sleep 1
done
DYN_HOME=$holder
DYN_GEN=$(room_field "$DYN" '.status.lease.generation')
[ -n "${PORT[$DYN_HOME]-}" ] || fail "dynamic room lease holder '$DYN_HOME' is not one of the relay pods"
DYN_KEY=$(room_field "$DYN" '.status.key')
statusz "$DYN_HOME" | jq -e --arg k "$DYN_KEY" '.rooms[$k].role == "home" and .rooms[$k].attachments == 1' >/dev/null \
  || fail "home pod $DYN_HOME does not publish the dynamic room $DYN_KEY with its attachment"
echo "PASS: dynamic room $DYN minted by pubsim — CR kind dynamic, attachment $BID, lease on $DYN_HOME (generation $DYN_GEN), key $DYN_KEY on /statusz"

# ---------------------------------------------- 5. kill the home, re-dial
# No grace: a crash, not a drain. The lease stays written with the dead pod's
# name and goes stale on its own clock, so the adoption path under test is
# force-take-on-stale, not release-on-drain (the transport suite covers that
# one). Joins during the stale window are proxied to a dead address and come
# back 503 — that is expected, and why this is a retry loop.
kubectl -n "$NS" delete pod "$DYN_HOME" --grace-period=0 --force --wait=false >/dev/null
echo "killed the dynamic room's home pod $DYN_HOME; re-dialling until the room is whole"
SURVIVOR=""
for p in "${PODS[@]}"; do [ "$p" != "$DYN_HOME" ] && SURVIVOR=$p; done

end=$((SECONDS + DEADLINE))
attempt=0
while true; do
  attempt=$((attempt + 1))
  LOG="$OUT/roomsim-rejoin-$attempt.out"
  "$ROOMSIM" -url "$RELAY_URL" -insecure -code "$CODE" -nick rejoin -duration 4s > "$LOG" 2> "$LOG.err" || true
  st=$(first_state "$LOG")
  if [ -n "$st" ] && jq -e --arg b "$BID" '.attachments | map(.broadcastID) | index($b) != null' <<<"$st" >/dev/null; then
    break
  fi
  [ "$SECONDS" -ge "$end" ] && fail "after ${DEADLINE}s and $attempt re-dials no joiner landed in $DYN with attachment $BID (last: $(cat "$LOG.err" "$LOG" 2>/dev/null | tail -n 3 | tr '\n' ' '))"
  sleep 2
done
NEW_HOME=$(room_field "$DYN" '.status.lease.holder')
NEW_GEN=$(room_field "$DYN" '.status.lease.generation')
[ -n "$NEW_HOME" ] && [ "$NEW_HOME" != "$DYN_HOME" ] || fail "the room was joinable but its lease still names the dead pod ($NEW_HOME)"
[ "${NEW_GEN:-0}" -gt "${DYN_GEN:-0}" ] || fail "adoption did not bump the lease generation ($DYN_GEN -> $NEW_GEN)"
if [ "$NEW_HOME" = "$SURVIVOR" ]; then
  statusz "$SURVIVOR" | jq -e --arg k "$DYN_KEY" '.rooms[$k].role == "home" and .rooms[$k].attachments == 1' >/dev/null \
    || fail "the adopting pod $SURVIVOR does not publish $DYN_KEY as home with its attachment"
  echo "PASS: after the kill, re-dial $attempt landed in the room with attachment $BID whole; $SURVIVOR adopted it (generation $DYN_GEN -> $NEW_GEN, /statusz role home)"
else
  # The Deployment's replacement pod took it; its ops listener is not
  # forwarded here, so the CR is the evidence.
  echo "PASS: after the kill, re-dial $attempt landed in the room with attachment $BID whole; replacement pod $NEW_HOME adopted it (generation $DYN_GEN -> $NEW_GEN)"
fi

kill "$PUBSIM_PID" 2>/dev/null || true
wait "$PUBSIM_PID" 2>/dev/null || true
# Wait for the fleet to be two Ready pods again so the tiers after this one
# start from the state they expect.
kubectl -n "$NS" rollout status deployment -l app.kubernetes.io/name=gawk-server --timeout=180s >/dev/null 2>&1 \
  || kubectl -n "$NS" wait --for=condition=Ready pod -l app.kubernetes.io/name=gawk-server --timeout=180s >/dev/null
echo "PASS: R42 rooms — static CR joinable and ended by kubectl, dynamic room adopted after its home died, across ${#PODS[@]} pods"
