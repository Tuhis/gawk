#!/usr/bin/env bash
# R40 (docs/71 CF8, §9.7) — content flagging, end to end, on the compose
# stack's screening lane: two real samplers on the generated test model,
# three `gawk-pubsim -fixture magenta` broadcasts (45 s magenta, 45 s grey,
# looped), the relay's keyframe route, gawk-admin's flag lifecycle, and a
# webhook receiver standing in for the operator's pager.
#
# Run it against a stack brought up with the lane's override:
#
#     docker compose -f docker-compose.yml -f dev/compose.screening.yml up -d --build --wait
#     ./e2e/screening-assert.sh
#
# What it proves, in order (each step is a docs/71 claim no unit test can
# make, because each needs every component in the chain at once):
#
#   1. The shards split the fleet (D22): gawk-admin's two target lists are
#      disjoint and together cover every magenta broadcast, and each sampler
#      pod screens exactly its own list — per-pod metrics, not a log line.
#   2. A sustained magenta signal PAGES (D9, D12): content_flag.raised
#      reaches the pager, and the flag is open and pinned in the portal.
#   3. A dismissal MUTES (D10): the broadcast is marked muted in the target
#      list, its sampler's tripped POSTs are answered `muted`, and no new
#      page goes out for it while the mute lasts.
#   4. The grey phase CLEARS (D7, D9): the flag resolves `cleared`, and the
#      next magenta phase of the same publisher session raises a NEW flag
#      and pages again.
#   5. A missing shard is LOUD (D22): with one sampler stopped, gawk-admin
#      reports that shard missing within its 30 s bound, and stops when the
#      sampler returns.
#   6. Observe mode pages NO ONE (D16, D19): with the samplers switched to
#      observe, the same pattern produces content_flag.observed on the
#      opt-in channel only — never a page — and the stored samples replay
#      to would-trips (D20, D21), while a threshold under the floor is
#      refused.
#
# Needs bash, curl, jq and `docker compose` with the lane's override in
# COMPOSE_FILE (defaulted below when unset). Takes about six minutes, most
# of it waiting for the fixture's phases to turn.
set -euo pipefail

cd "$(dirname "$0")/.."
export COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml:dev/compose.screening.yml}"
ADMIN="${ADMIN:-http://127.0.0.1:8088}"
# The samplers' metrics, published from gawk-admin's namespace (the
# override's comment says why).
SAMPLER_METRICS=("http://127.0.0.1:9190/metrics" "http://127.0.0.1:9191/metrics")
RAISED=fi.ioio.gawk.content_flag.raised
RESOLVED=fi.ioio.gawk.content_flag.resolved
OBSERVED=fi.ioio.gawk.content_flag.observed

dump() {
    echo "--- diagnostics ---" >&2
    docker compose logs --tail 30 sampler-0 sampler-1 admin >&2 2>&1 || true
    echo "--- deliveries ---" >&2
    pages | tail -20 >&2 || true
}
fail() { echo "::error::screening-assert: $*" >&2; dump; exit 1; }
step() { printf '\n== %s\n' "$*"; }

# wait_for <seconds> <what> <command…>: poll every 2 s until the command
# succeeds, or fail naming what never happened.
wait_for() {
    local deadline=$((SECONDS + $1)) what=$2
    shift 2
    until "$@"; do
        ((SECONDS < deadline)) || fail "timed out after ${what}"
        sleep 2
    done
}

# Every delivery the sink has printed, one JSON object per line.
pages() { docker compose logs --no-log-prefix webhook-sink 2>/dev/null | grep '^{' || true; }

# The fake IdP's two script doors, through the portal's /idp dev proxy.
TOKEN=$(curl -fsS -X POST "$ADMIN/idp/mint" | jq -r .access_token)
flagger_token() {
    curl -fsS -X POST "$ADMIN/idp/token" \
        --data-urlencode grant_type=client_credentials \
        --data-urlencode client_id=gawk-sampler \
        --data-urlencode client_secret=dev-sampler-secret | jq -r .access_token
}
api() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }

# metric <url> <series>: the integer value of one exact series — name and
# label set as the exposition writes them — 0 when it is absent. Matched as a
# literal, not a regex: label sets are full of braces. A here-string, never
# `curl | grep -q`: grep exiting at its first match makes curl's next write
# fail, and pipefail reports the success as a failure (docs/gotchas.md).
metric() {
    local body
    body=$(curl -fsS "$1") || { echo 0; return; }
    awk -v s="$2 " 'index($0, s) == 1 { v = $NF; found = 1; exit } END { printf "%d\n", found ? v : 0 }' <<<"$body"
}
# metric_sum <url> <metric-name>: the integer sum over every label set.
metric_sum() {
    local body
    body=$(curl -fsS "$1") || { echo 0; return; }
    awk -v n="$2" '$1 == n || index($1, n "{") == 1 { s += $NF } END { printf "%d\n", s }' <<<"$body"
}
# iso <GNU date offset> <BSD date offset>: now plus an offset, in UTC ISO
# 8601, on GNU (CI, Linux) and BSD (macOS) date alike.
iso() { date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v"$2" +%Y-%m-%dT%H:%M:%SZ; }

# --- 0. The lane is up --------------------------------------------------------
step "0. the portal is ready and the magenta broadcasts are live"
wait_for 120 "waiting for the portal's /readyz" curl -fsS -o /dev/null "$ADMIN/readyz"
TOKEN=$(curl -fsS -X POST "$ADMIN/idp/mint" | jq -r .access_token)

magenta_ids() {
    local logged live
    logged=$(docker compose logs --no-log-prefix pubsim-magenta 2>/dev/null |
        sed -n 's/.*GAWK_PUBSIM_ID=\([A-Z0-9]*\).*/\1/p' | sort -u)
    live=$(api "$ADMIN/api/v1/broadcasts" | jq -r '.broadcasts[] | select(.publisherActive) | .id' | sort -u)
    comm -12 <(printf '%s\n' "$logged") <(printf '%s\n' "$live")
}
three_live() { [ "$(magenta_ids | grep -c .)" -ge 3 ]; }
wait_for 90 "waiting for three live magenta broadcasts" three_live
mapfile -t IDS < <(magenta_ids)
echo "magenta broadcasts: ${IDS[*]}"

# --- 1. Shards ----------------------------------------------------------------
step "1. the two shards split the fleet, and each pod screens its own share"
FT=$(flagger_token)
targets() { curl -fsS -H "Authorization: Bearer $FT" "$ADMIN/api/v1/screening/targets?shard=$1&of=2"; }
t0=$(targets 0)
t1=$(targets 1)
[ "$(jq -r .flagRoute <<<"$t0")" = true ] || fail "flagRoute is false with the dev pager configured (D12)"
s0=$(jq -r '.targets[].broadcastId' <<<"$t0" | sort -u)
s1=$(jq -r '.targets[].broadcastId' <<<"$t1" | sort -u)
both=$(comm -12 <(printf '%s\n' "$s0") <(printf '%s\n' "$s1") | grep -c . || true)
[ "$both" = 0 ] || fail "the shards overlap: $(comm -12 <(printf '%s\n' "$s0") <(printf '%s\n' "$s1") | tr '\n' ' ')"
for id in "${IDS[@]}"; do
    n=$( (printf '%s\n' "$s0"; printf '%s\n' "$s1") | grep -cx "$id" || true)
    [ "$n" = 1 ] || fail "$id is in $n shard lists, want exactly one"
done
for i in 0 1; do
    want=$(jq '[.targets[] | select(.exempt | not)] | length' <<<"$([ "$i" = 0 ] && echo "$t0" || echo "$t1")")
    url=${SAMPLER_METRICS[$i]}
    screened() { [ "$(metric "$url" 'gawk_sampler_targets{state="screened"}')" = "$want" ]; }
    wait_for 60 "waiting for sampler-$i to screen its $want broadcast(s)" screened
    if [ "$want" -gt 0 ]; then
        sampling() { [ "$(metric_sum "$url" gawk_sampler_samples_total)" -gt 0 ]; }
        wait_for 60 "waiting for sampler-$i to take a sample" sampling
    fi
    echo "sampler-$i screens $want broadcast(s)"
done

# --- 2. A page ----------------------------------------------------------------
step "2. a sustained magenta signal pages"
raised_ids() { pages | jq -r --arg t "$RAISED" 'select(.path == "/pager" and .type == $t) | .subject' | sort -u; }
two_paged() { [ "$(raised_ids | grep -cxF -f <(printf '%s\n' "${IDS[@]}") || true)" -ge 2 ]; }
wait_for 120 "waiting for two magenta broadcasts to page" two_paged
mapfile -t PAGED < <(raised_ids | grep -xF -f <(printf '%s\n' "${IDS[@]}"))
A=${PAGED[0]}
B=${PAGED[1]}
flag_of() { # the broadcast's unresolved flag-mode flag, as the portal lists it
    api "$ADMIN/api/v1/content-flags?state=unresolved&mode=flag" |
        jq -c --arg b "$1" '[.contentFlags[] | select(.broadcastId == $b)] | first // empty'
}
FA=$(flag_of "$A")
[ -n "$FA" ] || fail "$A paged but has no unresolved flag"
[ "$(jq -r .state <<<"$FA")" = open ] || fail "$A's flag is $(jq -r .state <<<"$FA"), want open"
FA_ID=$(jq -r .id <<<"$FA")
pinned=$(api "$ADMIN/api/v1/broadcasts" | jq -r --arg b "$A" '.broadcasts[] | select(.id == $b) | .contentFlag.id // empty')
[ "$pinned" = "$FA_ID" ] || fail "$A's broadcast row does not carry flag $FA_ID (got '${pinned}')"
echo "$A paged: flag $FA_ID, open, pinned"

# --- 3. Dismiss mutes ---------------------------------------------------------
step "3. dismissing $B mutes it"
FB=$(flag_of "$B")
[ -n "$FB" ] || fail "$B paged but has no unresolved flag"
dismissed=$(api -X POST -H 'Content-Type: application/json' -d '{}' \
    "$ADMIN/api/v1/content-flags/$(jq -r .id <<<"$FB")/dismiss")
[ "$(jq -r .resolution <<<"$dismissed")" = dismissed ] || fail "dismiss answered $dismissed"
[ -n "$(jq -r '.mutedUntil // empty' <<<"$dismissed")" ] || fail "a dismissed flag carries no mutedUntil"
b_raises() { pages | jq -r --arg t "$RAISED" --arg b "$B" 'select(.path == "/pager" and .type == $t and .subject == $b) | .data.flagId' | sort -u | grep -c . || true; }
B_PAGES=$(b_raises)
FT=$(flagger_token)
b_muted() { [ "$( (targets 0; targets 1) | jq -r --arg b "$B" '.targets[] | select(.broadcastId == $b) | .muted')" = true ]; }
wait_for 10 "waiting for $B to be listed muted" b_muted
echo "$B dismissed and listed muted"

# --- 4. Clear, and a new episode ----------------------------------------------
step "4. the grey phase clears $A's flag; the next magenta phase pages again"
a_cleared() {
    pages | jq -e --arg t "$RESOLVED" --argjson id "$FA_ID" \
        'select(.path == "/pager" and .type == $t and .data.flagId == $id and .data.resolution == "cleared")' >/dev/null
}
wait_for 120 "waiting for flag $FA_ID to resolve cleared" a_cleared
a_new_page() {
    pages | jq -e --arg t "$RAISED" --arg b "$A" --argjson id "$FA_ID" \
        'select(.path == "/pager" and .type == $t and .subject == $b and .data.flagId != $id)' >/dev/null
}
wait_for 120 "waiting for $A's next magenta phase to raise a new flag" a_new_page
echo "$A cleared on grey and paged again on magenta"
# The magenta broadcasts started together, so by the time A has been through
# a whole grey phase and back, B has had a magenta phase of its own under the
# mute: its sampler's tripped POSTs were answered `muted`, and nothing paged.
muted_posts=0
for url in "${SAMPLER_METRICS[@]}"; do
    n=$(metric "$url" 'gawk_sampler_flag_posts_total{state="tripped",status="muted"}')
    muted_posts=$((muted_posts + n))
done
[ "$muted_posts" -gt 0 ] || fail "no tripped POST was answered muted — the mute was never exercised"
[ "$(b_raises)" = "$B_PAGES" ] || fail "$B paged again during its mute"
echo "$B's window tripped under the mute ($muted_posts muted POSTs) and paged no one"

# --- 5. Missing shard ---------------------------------------------------------
step "5. a stopped sampler is reported missing, and found again"
missing() { api "$ADMIN/api/v1/content-flags" | jq -c '.screening.missingShards'; }
[ "$(missing)" = "[]" ] || fail "shards missing with both samplers up: $(missing)"
docker compose stop sampler-1 >/dev/null
shard1_missing() { [ "$(missing)" = "[1]" ]; }
wait_for 75 "waiting for shard 1 to be reported missing" shard1_missing
docker compose start sampler-1 >/dev/null
none_missing() { [ "$(missing)" = "[]" ]; }
wait_for 60 "waiting for shard 1 to be found again" none_missing
echo "shard 1 went missing within the bound and came back"

# --- 6. Observe mode ----------------------------------------------------------
step "6. observe mode pages no one, and its samples replay"
switched=$(iso now +0S)
SAMPLER_MODE=observe docker compose up -d --no-deps sampler-0 sampler-1 >/dev/null
observed() {
    pages | jq -e --arg t "$OBSERVED" 'select(.path == "/observed" and .type == $t)' >/dev/null
}
wait_for 150 "waiting for content_flag.observed on the opt-in channel" observed
if pages | jq -e --arg t "$OBSERVED" 'select(.path == "/pager" and .type == $t)' >/dev/null; then
    fail "content_flag.observed reached the unfiltered pager (it is opt-in, D16)"
fi
if pages | jq -e --arg t "$RAISED" --arg s "$switched" 'select(.path == "/pager" and .type == $t and .time >= $s)' >/dev/null; then
    fail "a page went out after the samplers switched to observe"
fi
n_observed=$(api "$ADMIN/api/v1/content-flags?state=all&mode=observe" | jq '.contentFlags | length')
[ "$n_observed" -gt 0 ] || fail "the portal lists no observed flags"
from=$(iso '-1 hour' -1H)
to=$(iso '+5 minutes' +5M)
replay=$(api -X POST -H 'Content-Type: application/json' \
    -d "{\"threshold\":0.8,\"window\":6,\"windowHits\":4,\"from\":\"$from\",\"to\":\"$to\"}" \
    "$ADMIN/api/v1/screening/replay")
[ "$(jq '.wouldTrips' <<<"$replay")" -gt 0 ] || fail "replay found no would-trips: $replay"
[ "$(jq '.samplesStored' <<<"$replay")" -gt 0 ] || fail "replay found no stored samples: $replay"
status=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X POST \
    -d "{\"threshold\":0.1,\"window\":6,\"windowHits\":4,\"from\":\"$from\",\"to\":\"$to\"}" \
    "$ADMIN/api/v1/screening/replay")
[ "$status" = 400 ] || fail "a threshold under the floor answered $status, want 400 (D21)"
echo "observed: $n_observed flag(s), opt-in channel only; replay: $(jq -c '{wouldTrips, samplesStored, broadcastHours}' <<<"$replay")"

# Leave the lane as it was found.
docker compose up -d --no-deps sampler-0 sampler-1 >/dev/null

echo
echo "screening-assert: every property held"
