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
#   4. An IP ban makes a fresh publish attempt fail, and the relay counts it as
#      `gawk_connections_total{route="publish",outcome="banned"}` — the 451
#      rejection observed from outside the process.
#   5. Deleting both bans restores service: the gauges return to zero on every
#      pod and a fresh mint succeeds. An enforcement mechanism that cannot be
#      switched off is a worse bug than one that never switched on.
#
# What it deliberately does NOT assert: that close code 4006 reached viewers.
# Nothing in the harness observes close codes today (gawk-loadgen holds the
# session but never reports why it ended), so scraping `kubectl logs` would be
# the only option and that proves the log line, not the wire. The honest
# version of that assertion is a `-expect-close-code` flag on gawk-loadgen; it
# is written down here rather than faked. Unit coverage for 4006 is AP1's and
# AP3's, per-client and per-role.
#
# Usage: moderation-assert.sh <namespace> <broadcast-id> <admin-token> [deadline-seconds]
set -euo pipefail

# Paths resolve from this script's own directory, not the caller's cwd: the
# workflow runs it from the repo root, a human debugging it may not.
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBSIM="$E2E_DIR/bin/gawk-pubsim"
OUT="$E2E_DIR/out"
mkdir -p "$OUT"

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
declare -A PORT
i=0
for p in "${PODS[@]}"; do
  PORT[$p]=$((21400 + i))
  kubectl -n "$NS" port-forward "pod/$p" "${PORT[$p]}:2112" >/dev/null 2>&1 &
  i=$((i + 1))
done
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT
sleep 2

statusz() { curl -fsS --max-time 5 "http://127.0.0.1:${PORT[$1]}/statusz"; }
metrics() { curl -fsS --max-time 5 "http://127.0.0.1:${PORT[$1]}/metrics"; }
admin()   { curl -sS --max-time 5 -o "$2" -w '%{http_code}' \
              -H "Authorization: Bearer ${3-$TOKEN}" \
              "http://127.0.0.1:${PORT[$1]}/internal/admin/broadcasts"; }

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

# --------------------------------------------- 3. an ID ban kills, fleet-wide
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

# ------------------------------------------------- 4. an IP ban yields 451
# The publish path's rejection, observed from outside the process. Skipped
# only if the admin API reported no publisher IP, which would itself mean the
# broadcast had already ended — and step 3 asserts it was live before the ban.
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

  if "$PUBSIM" -url https://127.0.0.1:4433 -insecure -duration 8s \
       > "$OUT/pubsim-banned.out" 2> "$OUT/pubsim-banned.err"; then
    fail "gawk-pubsim published successfully from a banned IP ($CIDR)"
  fi
  after=0
  for p in "${PODS[@]}"; do
    after=$((after + $(metric_sum "$p" 'gawk_connections_total{' 'outcome="banned"')))
  done
  [ "$after" -gt "$before" ] || fail "the publish attempt failed but no pod counted outcome=\"banned\" ($before -> $after) — it failed for some other reason"
  echo "PASS: IP ban — publish from $CIDR refused, outcome=\"banned\" $before -> $after across the fleet"
else
  fail "the admin API reported no publisherRemoteIp for the live broadcast $BID"
fi

# ------------------------------------------------------- 5. unban restores
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

"$PUBSIM" -url https://127.0.0.1:4433 -insecure -duration 8s \
  > "$OUT/pubsim-unbanned.out" 2> "$OUT/pubsim-unbanned.err" \
  || fail "a fresh mint still fails after the bans were deleted: $(tail -5 "$OUT/pubsim-unbanned.err")"
grep -q GAWK_PUBSIM_ID "$OUT/pubsim-unbanned.out" \
  || fail "the post-unban publisher never minted an ID"
echo "PASS: unban — every pod's gauge back to 0 and a fresh mint succeeds"

echo "PASS: R39 fleet-wide kill, 451 enforcement and unban, across ${#PODS[@]} pods"
