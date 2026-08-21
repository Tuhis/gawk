#!/usr/bin/env bash
# R39 (docs/42 §4.14) — the fleet-wide kill, asserted on a real two-pod cluster.
#
# This is the one claim R39 cannot make from unit tests: a Ban custom resource
# applied to the API server reaches EVERY relay pod's informer and every pod
# acts on it. `cluster-assert.sh` has already established that the broadcast is
# origin on one pod and edge on the other, so "both pods dropped it" is a
# statement about the cascade, not about one process.
#
# What it proves, in order:
#
#   1. `kubectl get bans` works — the break-glass surface an operator reaches
#      for with gawk-admin down is real, not a design intention.
#   2. The relay admin API (§4.5) answers on the ops listener with the token
#      and 401s without it, and its raw-ID → HMAC-key mapping agrees with the
#      key /statusz publishes. That mapping is also how this script finds the
#      broadcast to assert on, since /statusz is HMAC-only by design.
#   3. An ID ban kills the broadcast on BOTH pods: its key leaves /statusz
#      everywhere, and `gawk_moderation_terminations_total` reaches ≥1 on each
#      pod. The counter is the fleet-wide-kill proof /statusz alone cannot
#      give — a hub can also vanish by expiring.
#   4. Close code 4006 reaches VIEWERS, on the edge pod as well as the origin.
#      Synthetic viewers attach through `gawk-loadgen -expect-close-code 4006`
#      first and the script waits until BOTH pods report subscribers for the
#      broadcast, so the sessions the kill has to close are spread across the
#      cascade — which is the point of TerminateBroadcast closing internal
#      edge sessions and local subscribers on every pod, not just the origin's.
#      loadgen reads the code off the session itself, so this is the wire, not
#      a log line: 4000 (broadcast ended), a transport death with no code at
#      all, and a session nothing ever closed each fail differently.
#   5. An IP ban makes a fresh publish attempt fail with a READABLE 451:
#      `gawk-pubsim` reports `GAWK_PUBSIM_DIAL_STATUS=451` and exits 3, and the
#      relay counts `gawk_connections_total{route="publish",outcome="banned"}`.
#      Asserting the status rather than "it failed" is the whole of docs/42
#      D15 — 451 was chosen over reusing 403 precisely so a native broadcaster
#      can say "banned" instead of "auth failed", and a harness that only sees
#      a failed dial proves the rejection but never that property.
#   6. Deleting both bans restores service: the gauges return to zero on every
#      pod and a fresh mint succeeds. An enforcement mechanism that cannot be
#      switched off is a worse bug than one that never switched on.
#
# Unit coverage for 4006 is still AP1's and AP3's, per-client and per-role;
# what only this tier can add is that the code crosses a real cascade.
#
# Usage: moderation-assert.sh <namespace> <broadcast-id> <admin-token> [deadline-seconds]
set -euo pipefail

# Paths resolve from this script's own directory, not the caller's cwd: the
# workflow runs it from the repo root, a human debugging it may not.
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBSIM="$E2E_DIR/bin/gawk-pubsim"
LOADGEN="$E2E_DIR/bin/gawk-loadgen"
OUT="$E2E_DIR/out"
mkdir -p "$OUT"
# The media port is UDP and reaches the fleet through kind's NodePort mapping;
# only the ops endpoints need port-forwarding. Same URL the job's own pubsim
# and loadgen invocations use.
RELAY_URL=${GAWK_E2E_URL:-https://127.0.0.1:4433}

NS=${1:?usage: moderation-assert.sh <namespace> <broadcast-id> <admin-token> [deadline]}
BID=${2:?usage: moderation-assert.sh <namespace> <broadcast-id> <admin-token> [deadline]}
TOKEN=${3:?usage: moderation-assert.sh <namespace> <broadcast-id> <admin-token> [deadline]}
DEADLINE=${4:-90}

mapfile -t PODS < <(kubectl -n "$NS" get pods -l app.kubernetes.io/name=gawk-server \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if [ "${#PODS[@]}" -ne 2 ]; then
  echo "expected 2 relay pods in $NS, found ${#PODS[@]}: ${PODS[*]-}" >&2
  exit 1
fi

# Same side-channel cluster-assert.sh uses: the ops listener is TCP and
# port-forwards fine, while the media port is UDP and does not.
#
# SUPERVISED, not fire-and-forget. `kubectl port-forward` binds
# asynchronously — on a loaded runner that can take well over the 2s a fixed
# sleep would allow, and the first assertion below would then fail on curl
# exit 7 for a reason that has nothing to do with moderation. Worse, it EXITS
# on a broken connection, which is plausible exactly when the kill step tears
# hubs down: without a restart, every later call against that pod fails for
# the rest of the run and step 4 burns its whole deadline before FAILing a
# kill that actually propagated.
#
# So: one supervisor per pod that re-dials forever, and a bounded readiness
# poll against /healthz (always 200 on the ops listener, unlike /readyz which
# 503s during a drain) before anything is asserted.
declare -A PORT
forward() { # forward <pod> <local-port> — runs in the background, re-dials forever
  local pod=$1 port=$2 kid=
  # Traps are reset to default in a background subshell, so the supervisor
  # sets its own: without this the EXIT trap below would orphan the kubectl.
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
  PORT[$p]=$((21400 + i))
  forward "$p" "${PORT[$p]}" &
  i=$((i + 1))
done
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

# --retry-connrefused covers the restart gap: a request that lands in the
# second between one kubectl dying and its successor binding is retried rather
# than reported as a pod that stopped answering. --retry does NOT retry a 4xx,
# so the deliberate 401 assertion below still sees its 401 on the first try.
CURL=(curl -sS --max-time 5 --retry 3 --retry-delay 1 --retry-connrefused)
statusz() { "${CURL[@]}" -f "http://127.0.0.1:${PORT[$1]}/statusz"; }
metrics() { "${CURL[@]}" -f "http://127.0.0.1:${PORT[$1]}/metrics"; }
admin()   { "${CURL[@]}" -o "$2" -w '%{http_code}' \
              -H "Authorization: Bearer ${3-$TOKEN}" \
              "http://127.0.0.1:${PORT[$1]}/internal/admin/broadcasts"; }

FORWARD_WAIT=60
for p in "${PODS[@]}"; do
  end=$((SECONDS + FORWARD_WAIT))
  # -s without -S: a not-yet-bound forward is the EXPECTED state here, and one
  # "Could not connect" line per second is noise, not a diagnostic. The one
  # attempt that matters — the last — is repeated loudly on the way out.
  until curl -fs --max-time 3 -o /dev/null "http://127.0.0.1:${PORT[$p]}/healthz"; do
    if [ "$SECONDS" -ge "$end" ]; then
      echo "port-forward to $p (127.0.0.1:${PORT[$p]} -> 2112) never answered /healthz within ${FORWARD_WAIT}s" >&2
      curl -sS --max-time 3 -o /dev/null "http://127.0.0.1:${PORT[$p]}/healthz" >&2 || true
      exit 1
    fi
    sleep 1
  done
done
echo "PASS: ops port-forwards ready on all ${#PODS[@]} pods (/healthz answered)"

# metric_sum <pod> <metric name> [label substring]
# Sums every sample line for that metric, optionally narrowed to lines
# containing the label substring. Absent metric = 0, which is right: a counter
# that has never been incremented may legitimately not be exported at all.
# Plain string matching rather than a regex — `{` and `"` in a dynamic awk
# regex are a portability trap for no gain here.
metric_sum() {
  metrics "$1" 2>/dev/null | awk -v name="$2" -v lbl="${3-}" '
    /^#/ { next }
    index($0, name) != 1 { next }
    lbl != "" && index($0, lbl) == 0 { next }
    { s += $NF }
    END { printf "%d", s + 0 }'
}

fail() {
  echo "FAIL: $*" >&2
  for p in "${PODS[@]}"; do
    echo "--- $p /statusz:" >&2
    statusz "$p" >&2 || echo "(unreachable)" >&2
    echo "--- $p moderation metrics:" >&2
    metrics "$p" 2>/dev/null | grep -E '^gawk_moderation|outcome="banned"' >&2 || echo "(none)" >&2
  done
  kubectl -n "$NS" get bans -o yaml >&2 2>&1 || true
  exit 1
}

# ---------------------------------------------------------------- 1. the CRD
# The emergency surface itself: with gawk-admin down (it is not even installed
# here), `kubectl get bans` has to resolve and list.
kubectl -n "$NS" get bans >/dev/null || fail "the Ban CRD is not installed — moderation.enabled did not render it"
echo "PASS: kubectl get bans resolves (the break-glass surface exists)"

# ------------------------------------------------------- 2. the admin API
# 401 without a credential, 200 with it, and a raw ID present — the scoped
# relaxation of the never-expose-raw-IDs invariant (docs/42 D8), which is only
# acceptable because this listener is ClusterIP-only AND credential-gated.
ADMIN_JSON="$(mktemp)"
code=$(admin "${PODS[0]}" "$ADMIN_JSON" "definitely-not-the-token" || true)
[ "$code" = "401" ] || fail "the relay admin API answered $code to a wrong bearer token (want 401)"

KEY=""
PUBLISHER_IP=""
for p in "${PODS[@]}"; do
  code=$(admin "$p" "$ADMIN_JSON" || true)
  [ "$code" = "200" ] || fail "GET /internal/admin/broadcasts on $p answered $code (want 200)"
  k=$(jq -r --arg id "$BID" '.broadcasts[]? | select(.id == $id) | .key' "$ADMIN_JSON" | head -1)
  if [ -n "$k" ] && [ "$k" != "null" ]; then
    KEY=$k
    ip=$(jq -r --arg id "$BID" '.broadcasts[]? | select(.id == $id and .publisherActive) | .publisherRemoteIp // empty' \
           "$ADMIN_JSON" | head -1)
    [ -n "$ip" ] && PUBLISHER_IP=$ip
  fi
done
[ -n "$KEY" ] || fail "no pod reported broadcast $BID on /internal/admin/broadcasts"
# The mapping has to agree with what /statusz publishes, or the portal's
# telemetry deep link would point at a broadcast nobody else can name.
seen=0
for p in "${PODS[@]}"; do
  statusz "$p" | jq -e --arg k "$KEY" '.broadcasts | has($k)' >/dev/null 2>&1 && seen=1
done
[ "$seen" -eq 1 ] || fail "the admin API's key $KEY for $BID does not appear on any pod's /statusz"
echo "PASS: admin API — 401 without the token, raw id $BID <-> key $KEY consistent with /statusz${PUBLISHER_IP:+, publisher $PUBLISHER_IP}"

# --------------------------------- 3. observers that will report the 4006
# Viewers that report WHY they were closed, attached before the ban so the
# kill has real subscriber sessions to close on both pods.
#
# 8 sessions, not more: the job's own `gawk-loadgen -viewers 12` + `-viewers 6`
# may still be holding slots against the chart's per-pod maxSubscribers (15),
# and an observer whose dial was refused for capacity would fail this
# assertion for a reason that has nothing to do with 4006.
#
# -duration is a ceiling, not a wait: loadgen exits as soon as every session
# has ended, so on the happy path this costs the kill's own latency. It also
# exits non-zero if it is still holding open sessions when the ceiling
# arrives — the failure "the kill never reached this viewer" — so the ceiling
# has to outlive the spread wait plus the kill deadline below, with slack.
OBSERVERS=8
SPREAD_WAIT=45
OBSERVER_PID=""
OBSERVER_ATTEMPT=0
start_observers() {
  if [ -n "$OBSERVER_PID" ]; then
    # REPLACE the batch rather than add to it: this only happens when every
    # session landed on one pod, so those sessions are re-rolling the same
    # conntrack dice from fresh 5-tuples — and freeing their slots first
    # keeps the subscriber cap out of the picture. SIGTERM, so loadgen still
    # writes its log.
    kill "$OBSERVER_PID" 2>/dev/null || true
    wait "$OBSERVER_PID" 2>/dev/null || true
  fi
  OBSERVER_ATTEMPT=$((OBSERVER_ATTEMPT + 1))
  "$LOADGEN" -url "$RELAY_URL" -id "$BID" -viewers "$OBSERVERS" -ramp-ms 150 \
    -insecure -duration "$((DEADLINE + SPREAD_WAIT + 60))s" -expect-close-code 4006 \
    > "$OUT/loadgen-4006-$OBSERVER_ATTEMPT.log" 2>&1 &
  OBSERVER_PID=$!
}

# Both pods must actually be serving some of them: kube-proxy spreads UDP by
# 5-tuple, so placement is probabilistic and has to be observed rather than
# assumed. Without this the assertion could pass with every observer on the
# origin, and the edge half of the fan-out — TerminateBroadcast closing local
# subscribers on a pod that only ever pulled the broadcast — would go untested.
# Subscribers for KEY summed across the fleet. Counts every kind the relay
# reports — including the edge's internal pull session and any viewers the
# earlier tiers left on this broadcast — which is why the wait below is
# expressed against a BASELINE taken before the observers start rather than
# against an absolute number.
observers_total() {
  local p n sum=0
  for p in "${PODS[@]}"; do
    n=$(statusz "$p" 2>/dev/null | jq -r --arg k "$KEY" '.broadcasts[$k].subscribers // 0' 2>/dev/null)
    [ -n "$n" ] || n=0
    sum=$((sum + n))
  done
  printf '%s' "$sum"
}

# Two conditions, and BOTH matter.
#
# Spread: at least one observer on every pod, or the edge half of the fan-out —
# TerminateBroadcast closing local subscribers on a pod that only ever pulled
# the broadcast — goes untested.
#
# Count: every observer actually connected. Waiting only for spread is what run
# 32499235861 did, and it is satisfied the moment TWO of eight are up: the ban
# then lands mid-ramp, the remaining six dial a broadcast that no longer exists,
# and loadgen correctly reports "7 session(s) never connected" — a real failure
# of the harness, indistinguishable at a glance from the wire regression this
# step exists to catch.
observers_spread() {
  local p spread=0
  for p in "${PODS[@]}"; do
    statusz "$p" 2>/dev/null \
      | jq -e --arg k "$KEY" '(.broadcasts[$k].subscribers // 0) >= 1' >/dev/null 2>&1 \
      && spread=$((spread + 1))
  done
  [ "$spread" -eq "${#PODS[@]}" ] && [ "$(observers_total)" -ge "$OBSERVER_TARGET" ]
}

OBSERVER_BASELINE=$(observers_total)
OBSERVER_TARGET=$((OBSERVER_BASELINE + OBSERVERS))
echo "observers: baseline $OBSERVER_BASELINE subscriber(s) for $KEY, waiting for $OBSERVER_TARGET"
start_observers
end=$((SECONDS + SPREAD_WAIT))
while ! observers_spread; do
  if [ "$SECONDS" -ge "$end" ]; then
    # One re-roll, the cluster-assert.sh precedent: P(all 8 on one pod) is
    # ~2⁻⁷ per attempt, so a retry is far cheaper than a rerun of the tier.
    if [ "$OBSERVER_ATTEMPT" -lt 2 ]; then
      echo "conntrack put every observer on one pod; re-rolling $OBSERVERS sessions"
      start_observers
      end=$((SECONDS + SPREAD_WAIT))
      continue
    fi
    fail "after $OBSERVER_ATTEMPT attempts the observers are not both spread and complete for $KEY (now $(observers_total), want >= $OBSERVER_TARGET with >= 1 on each of ${#PODS[@]} pods) — the 4006 fan-out cannot be observed across the cascade (check for refused dials in $OUT/loadgen-4006-*.log)"
  fi
  sleep 2
done
echo "PASS: $OBSERVERS observing viewers attached (attempt $OBSERVER_ATTEMPT), $(observers_total) subscriber(s) for $KEY spread across all ${#PODS[@]} pods"

# --------------------------------------------- 4. an ID ban kills, fleet-wide
ID_BAN="ban-id-$(printf '%s' "$BID" | tr '[:upper:]' '[:lower:]')"
EXPIRES=$(date -u -d '+10 minutes' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
          || date -u -v+10M +%Y-%m-%dT%H:%M:%SZ)

kubectl -n "$NS" apply -f - <<EOF
apiVersion: gawk.ioio.fi/v1alpha1
kind: Ban
metadata:
  name: $ID_BAN
spec:
  target:
    type: broadcastId
    value: "$BID"
  expiresAt: "$EXPIRES"
  reason: "e2e-cluster: fleet-wide kill assertion"
  createdBy: "kubectl"
EOF
kubectl -n "$NS" get bans

end=$((SECONDS + DEADLINE))
while true; do
  gone=1
  killed=0
  for p in "${PODS[@]}"; do
    st=$(statusz "$p" 2>/dev/null) || { gone=0; continue; }
    if jq -e --arg k "$KEY" '.broadcasts | has($k)' >/dev/null <<<"$st"; then gone=0; fi
    n=$(metric_sum "$p" 'gawk_moderation_terminations_total')
    [ "$n" -ge 1 ] && killed=$((killed + 1))
  done
  # BOTH conditions, and both across BOTH pods. The disappearance alone is
  # ambiguous (a hub can expire); the counter alone does not prove the hub is
  # actually gone.
  if [ "$gone" -eq 1 ] && [ "$killed" -eq "${#PODS[@]}" ]; then break; fi
  [ "$SECONDS" -ge "$end" ] && fail "after ${DEADLINE}s: broadcast $KEY still present on some pod (gone=$gone) or only $killed/${#PODS[@]} pods counted a termination"
  sleep 2
done
echo "PASS: ID ban — $KEY gone from BOTH pods' /statusz, gawk_moderation_terminations_total >= 1 on each"

# The informer's view, per pod: the gauge is what says the ban is in force
# here, as opposed to merely existing in etcd.
for p in "${PODS[@]}"; do
  n=$(metric_sum "$p" 'gawk_moderation_bans_active')
  [ "$n" -ge 1 ] || fail "pod $p reports gawk_moderation_bans_active=$n after the ID ban — its informer did not see the CR"
done
echo "PASS: every pod's informer holds the ban (gawk_moderation_bans_active >= 1)"

# The wire, not the log line: every observing session must have been closed by
# the relay with 4006, and loadgen names the code it got instead when one was
# not. Sessions on the edge pod count the same as the origin's, which is the
# claim TerminateBroadcast's per-pod fan-out exists to make.
#
# One ordering caveat, written down because a flake here is otherwise
# baffling: the origin's own terminate deletes the cluster Lease, and a pod
# that saw the lease deletion BEFORE its own Ban event would expire the hub
# through the ordinary path, closing its viewers with 4000. The Ban event has
# a head start of the origin's entire handler plus an API round trip, so 4006
# is what lands — but if this ever reports 4000 on one pod, that race is the
# first place to look, not a wire regression.
rc=0
OBSERVER_LOG="$OUT/loadgen-4006-$OBSERVER_ATTEMPT.log"
wait "$OBSERVER_PID" || rc=$?
# loadgen's verdict block is everything from its first `gawk-loadgen:` line on;
# the load report above it is noise here and stays in the uploaded artifact.
[ "$rc" -eq 0 ] || fail "gawk-loadgen -expect-close-code 4006 exited $rc: $(sed -n '/^gawk-loadgen: /,$p' "$OBSERVER_LOG" | tr '\n' ' ')(full log: $OBSERVER_LOG)"
echo "PASS: 4006 reached every observing viewer across all ${#PODS[@]} pods (read off the session, not from a log)"

# ------------------------------------------------- 5. an IP ban yields 451
# The publish path's rejection, observed from outside the process. Skipped
# only if the admin API reported no publisher IP, which would itself mean the
# broadcast had already ended — and step 4 asserts it was live before the ban.
IP_BAN=""
if [ -n "$PUBLISHER_IP" ]; then
  case "$PUBLISHER_IP" in
    *:*) CIDR="$PUBLISHER_IP/128" ;;
    *)   CIDR="$PUBLISHER_IP/32" ;;
  esac
  IP_BAN="ban-ip-$(printf '%s' "$CIDR" | sha256sum | cut -c1-12)"
  before=0
  for p in "${PODS[@]}"; do
    before=$((before + $(metric_sum "$p" 'gawk_connections_total{' 'outcome="banned"')))
  done
  kubectl -n "$NS" apply -f - <<EOF
apiVersion: gawk.ioio.fi/v1alpha1
kind: Ban
metadata:
  name: $IP_BAN
spec:
  target:
    type: ip
    value: "$CIDR"
  expiresAt: "$EXPIRES"
  reason: "e2e-cluster: 451 on the publish path"
  createdBy: "kubectl"
EOF
  # Give every pod's informer a moment; the ban must be in force wherever
  # conntrack sends the dial.
  for _ in $(seq 30); do
    ready=0
    for p in "${PODS[@]}"; do
      [ "$(metric_sum "$p" 'gawk_moderation_bans_active')" -ge 2 ] && ready=$((ready + 1))
    done
    [ "$ready" -eq "${#PODS[@]}" ] && break
    sleep 1
  done

  rc=0
  "$PUBSIM" -url "$RELAY_URL" -insecure -duration 8s \
    > "$OUT/pubsim-banned.out" 2> "$OUT/pubsim-banned.err" || rc=$?
  [ "$rc" -ne 0 ] || fail "gawk-pubsim published successfully from a banned IP ($CIDR)"
  # The status, not merely the failure. docs/42 D15 chose 451 over reusing 403
  # so a NATIVE broadcaster can tell "banned" from "auth failed" — a property
  # that is only real if something reads the status, and only pubsim can (a
  # browser sees an opaque dial failure, docs/22 finding 12). Exit code 3 is
  # pubsim's "the relay refused the dial with an HTTP status"; the status line
  # says which status, and 401/404/409/429 would all fail this check.
  grep -qx 'GAWK_PUBSIM_DIAL_STATUS=451' "$OUT/pubsim-banned.err" && [ "$rc" -eq 3 ] \
    || fail "the publish from $CIDR failed (exit $rc) but gawk-pubsim did not report a readable 451: $(grep -m1 GAWK_PUBSIM_DIAL_STATUS "$OUT/pubsim-banned.err" || echo 'no GAWK_PUBSIM_DIAL_STATUS line at all')"
  after=0
  for p in "${PODS[@]}"; do
    after=$((after + $(metric_sum "$p" 'gawk_connections_total{' 'outcome="banned"')))
  done
  [ "$after" -gt "$before" ] || fail "the publish attempt failed but no pod counted outcome=\"banned\" ($before -> $after) — it failed for some other reason"
  echo "PASS: IP ban — publish from $CIDR refused with a readable HTTP 451 (pubsim exit 3), outcome=\"banned\" $before -> $after across the fleet"
else
  fail "the admin API reported no publisherRemoteIp for the live broadcast $BID"
fi

# ------------------------------------------------------- 6. unban restores
kubectl -n "$NS" delete ban "$ID_BAN" ${IP_BAN:+"$IP_BAN"}
end=$((SECONDS + DEADLINE))
while true; do
  clear=0
  for p in "${PODS[@]}"; do
    [ "$(metric_sum "$p" 'gawk_moderation_bans_active')" -eq 0 ] && clear=$((clear + 1))
  done
  [ "$clear" -eq "${#PODS[@]}" ] && break
  [ "$SECONDS" -ge "$end" ] && fail "after ${DEADLINE}s only $clear/${#PODS[@]} pods have gawk_moderation_bans_active=0 — the delete did not propagate"
  sleep 2
done

"$PUBSIM" -url "$RELAY_URL" -insecure -duration 8s \
  > "$OUT/pubsim-unbanned.out" 2> "$OUT/pubsim-unbanned.err" \
  || fail "a fresh mint still fails after the bans were deleted: $(tail -5 "$OUT/pubsim-unbanned.err")"
grep -q GAWK_PUBSIM_ID "$OUT/pubsim-unbanned.out" \
  || fail "the post-unban publisher never minted an ID"
echo "PASS: unban — every pod's gauge back to 0 and a fresh mint succeeds"

echo "PASS: R39 fleet-wide kill, 4006 to viewers, readable 451 and unban, across ${#PODS[@]} pods"
