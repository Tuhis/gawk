#!/usr/bin/env bash
# R39 (docs/42 §11.1's kind-tier row) — one kill driven through the PORTAL API
# on a real cluster, closing what neither the fakes nor envtest can reach:
# the chart's actual RBAC. Every write below goes through gawk-admin's own
# ServiceAccount and Role against a real authorizer, so a Role missing a verb
# — the silent class where the reconciler stops projecting while the fleet
# enforces stale state — fails here by name.
#
# What it proves, in order:
#
#   1. The portal came up FOR REAL: the migrate hook ran against a real
#      Postgres on a waited first install, OIDC discovery resolved against the
#      in-cluster fake IdP, and /readyz answers 200.
#   2. Authentication is enforced end to end: /api/v1/me is 401 bare and 200
#      with a token the IdP minted, carrying the operator role.
#   3. The fleet view sees a live broadcast through relayscan — the headless
#      Service, the relay admin token, the whole D12 discovery chain.
#   4. A kill through POST /broadcasts/{id}/kill answers 201 — not 202, which
#      would mean the CR write FAILED — and the canonical Ban CR exists with
#      the ban-id annotation: CRClient.Upsert through real RBAC against the
#      real CRD schema.
#   5. A break-glass `kubectl apply` Ban is ADOPTED: the reconciler stamps the
#      operator's own CR (the patch verb) and the row appears in the portal's
#      ban list.
#   6. Unban through the API answers 204 and the CR is gone (the delete verb),
#      for both bans.
#
# Runs AFTER moderation-assert.sh: that script's charter says gawk-admin is
# not installed while IT runs (the break-glass surface must work without the
# portal), so the portal half of the tier installs later and mints its own
# fixture broadcast.
#
# Usage: admin-assert.sh <namespace> [deadline-seconds]
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBSIM="$E2E_DIR/bin/gawk-pubsim"
OUT="$E2E_DIR/out"
mkdir -p "$OUT"
RELAY_URL=${GAWK_E2E_URL:-https://127.0.0.1:4433}

NS=${1:?usage: admin-assert.sh <namespace> [deadline]}
DEADLINE=${2:-120}

ADMIN_PORT=18090
IDP_PORT=18091

# Supervised port-forwards, the moderation-assert pattern: kubectl binds
# asynchronously and exits on a broken connection, so each forward re-dials
# forever and readiness is polled, never slept for.
forward() { # forward <svc> <local-port> <remote-port>
  local svc=$1 port=$2 remote=$3 kid=
  trap 'kill "$kid" 2>/dev/null; exit 0' TERM INT
  while true; do
    kubectl -n "$NS" port-forward "svc/$svc" "$port:$remote" >/dev/null 2>&1 &
    kid=$!
    wait "$kid" 2>/dev/null || true
    sleep 1
  done
}
forward gawk-admin "$ADMIN_PORT" 8090 &
forward gawk-fakeidp "$IDP_PORT" 8080 &
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT

CURL=(curl -sS --max-time 10 --retry 3 --retry-delay 1 --retry-connrefused)

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --------------------------------------------------------- 1. the portal is up
# /readyz is the AND of "schema is one we can serve" and "OIDC resolved", so
# a 200 here is the migrate-hook-on-a-waited-install and discovery assertions
# in one probe.
ready=false
for _ in $(seq "$DEADLINE"); do
  if "${CURL[@]}" -o /dev/null -f "http://127.0.0.1:$ADMIN_PORT/readyz" 2>/dev/null; then
    ready=true
    break
  fi
  sleep 1
done
$ready || fail "gawk-admin never answered /readyz 200 (migrations, Postgres, or OIDC discovery)"
echo "PASS: gawk-admin is ready (schema migrated, OIDC discovery resolved)"

# ------------------------------------------------------------- 2. authn works
TOKEN=$("${CURL[@]}" -X POST "http://127.0.0.1:$IDP_PORT/mint" | jq -r .access_token)
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] || fail "the fake IdP minted no token"
AUTH=(-H "Authorization: Bearer $TOKEN")

code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' "http://127.0.0.1:$ADMIN_PORT/api/v1/me")
[ "$code" = "401" ] || fail "/api/v1/me answered $code without a token (want 401)"
ME=$("${CURL[@]}" -f "${AUTH[@]}" "http://127.0.0.1:$ADMIN_PORT/api/v1/me")
echo "$ME" | jq -e '.roles | index("operator")' >/dev/null \
  || fail "/api/v1/me does not carry the operator role: $ME"
echo "PASS: bare requests 401, the IdP's token authenticates with the operator role"

# ------------------------------------- 3. a live broadcast in the fleet view
"$PUBSIM" -url "$RELAY_URL" -insecure -duration 300s \
  > "$OUT/pubsim-admin.out" 2> "$OUT/pubsim-admin.err" &
PUBSIM_PID=$!
for _ in $(seq 100); do
  grep -q GAWK_PUBSIM_ID "$OUT/pubsim-admin.out" && break
  sleep 0.2
done
BID=$(grep -m1 -o 'GAWK_PUBSIM_ID=[A-Z0-9]*' "$OUT/pubsim-admin.out" | cut -d= -f2)
[ -n "$BID" ] || { cat "$OUT/pubsim-admin.err" >&2; fail "pubsim never minted a broadcast"; }

seen=false
for _ in $(seq "$DEADLINE"); do
  if "${CURL[@]}" -f "${AUTH[@]}" "http://127.0.0.1:$ADMIN_PORT/api/v1/broadcasts" \
    | jq -e --arg id "$BID" '.broadcasts[] | select(.id == $id)' >/dev/null 2>&1; then
    seen=true
    break
  fi
  sleep 1
done
$seen || fail "broadcast $BID never appeared in the portal fleet view (relayscan/D12)"
echo "PASS: the portal fleet view lists $BID through relayscan"

# ------------------------------------------------- 4. the kill, through RBAC
KILL=$("${CURL[@]}" -w '\n%{http_code}' "${AUTH[@]}" -H 'Content-Type: application/json' \
  -X POST -d '{"reason":"e2e kill through the portal","cooldownSeconds":60}' \
  "http://127.0.0.1:$ADMIN_PORT/api/v1/broadcasts/$BID/kill")
code=$(tail -n1 <<<"$KILL")
body=$(sed '$d' <<<"$KILL")
# 201, NOT 202: a 202 means the row committed but the CR write failed — on
# this tier that is precisely an RBAC or schema failure, the thing to catch.
[ "$code" = "201" ] || fail "kill answered $code (want 201 — a 202 means the CR write failed): $body"
BAN_ID=$(jq -r .ban.id <<<"$body")
CR_NAME=$(jq -r .ban.crName <<<"$body")
[ -n "$CR_NAME" ] && [ "$CR_NAME" != null ] || fail "the kill's ban carries no crName: $body"

kubectl -n "$NS" get ban "$CR_NAME" >/dev/null \
  || fail "the kill answered 201 but its Ban CR $CR_NAME does not exist"
anno=$(kubectl -n "$NS" get ban "$CR_NAME" -o jsonpath='{.metadata.annotations.gawk\.ioio\.fi/ban-id}')
[ "$anno" = "$BAN_ID" ] || fail "CR $CR_NAME carries ban-id annotation '$anno', want $BAN_ID"
echo "PASS: kill answered 201 and the real API server accepted CR $CR_NAME (create through the chart's Role)"

# The relay side actuates: the publisher session dies. pubsim was started
# with a 300 s duration, so an exit within the deadline is the kill, not the
# clock.
killed=false
for _ in $(seq "$DEADLINE"); do
  if ! kill -0 "$PUBSIM_PID" 2>/dev/null; then
    killed=true
    break
  fi
  sleep 1
done
$killed || fail "the publisher session outlived the portal kill"
echo "PASS: the publisher session ended after the portal kill"

# ----------------------------------------- 5. break-glass adoption, via patch
kubectl -n "$NS" apply -f - <<BAN
apiVersion: gawk.ioio.fi/v1alpha1
kind: Ban
metadata:
  name: e2e-breakglass-ban
  namespace: $NS
spec:
  target:
    type: broadcastId
    value: "ZZZ234"
  reason: applied with kubectl while proving adoption
  createdBy: kubectl
BAN
adopted=false
for _ in $(seq "$DEADLINE"); do
  a=$(kubectl -n "$NS" get ban e2e-breakglass-ban \
    -o jsonpath='{.metadata.annotations.gawk\.ioio\.fi/ban-id}' 2>/dev/null || true)
  if [ -n "$a" ]; then
    adopted=true
    ADOPTED_ID=$a
    break
  fi
  sleep 1
done
$adopted || fail "the reconciler never adopted the kubectl-applied Ban (the patch verb, or the sweep)"
"${CURL[@]}" -f "${AUTH[@]}" "http://127.0.0.1:$ADMIN_PORT/api/v1/bans?state=active" \
  | jq -e --arg id "$ADOPTED_ID" '.bans[] | select(.id == $id)' >/dev/null \
  || fail "the adopted ban $ADOPTED_ID is not in the portal's active list"
echo "PASS: the break-glass CR was adopted (stamped in place) and appears in the portal"

# ------------------------------------------------- 6. unban, through delete
for id in "$BAN_ID" "$ADOPTED_ID"; do
  code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' "${AUTH[@]}" -X DELETE \
    "http://127.0.0.1:$ADMIN_PORT/api/v1/bans/$id")
  [ "$code" = "204" ] || fail "unban of $id answered $code (want 204 — a 202 means the CR delete failed)"
done
gone=false
for _ in $(seq "$DEADLINE"); do
  n=$(kubectl -n "$NS" get bans -o name 2>/dev/null | wc -l)
  if [ "$n" -eq 0 ]; then
    gone=true
    break
  fi
  sleep 1
done
$gone || { kubectl -n "$NS" get bans >&2; fail "Ban CRs survived their unbans (the delete verb)"; }
echo "PASS: both unbans answered 204 and every Ban CR is gone"

echo "admin-assert: every portal-tier property held"
