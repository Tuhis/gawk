#!/bin/sh
# Static assertions about the local development stack (R38, docs/41).
#
# These are the properties that are decisions rather than behaviour — which
# address the ops listener is published on, which services a bare `up` starts,
# what is gitignored. None of them shows up in a runtime test, and each one is
# a line somebody could "tidy" without noticing what it was for.
#
# No dependencies beyond `docker compose` (parse only — no daemon needed) and
# git. Run from the repository root:
#
#     ./dev/stack_test.sh
set -eu

cd "$(dirname "$0")/.."
COMPOSE=docker-compose.yml
failures=0

ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }

check() { # check <description> <command…>
    desc=$1
    shift
    if "$@" >/dev/null 2>&1; then ok "$desc"; else fail "$desc"; fi
}

if ! docker compose version >/dev/null 2>&1; then
    echo "dev/stack_test.sh: needs \`docker compose\` (parse only, no daemon)" >&2
    exit 2
fi

# Every compose invocation ignores .env deliberately: these are assertions
# about the stack's DESIGN, and a developer who has switched to the ACME
# lane must not turn them red. It also means the inline defaults in
# docker-compose.yml — the ones a bare `docker compose up` on a fresh clone
# uses — are what gets checked.
dc() { docker compose --env-file /dev/null "$@"; }

echo "compose file"

# D14, and §5 states it as a literal instruction about this file: the ops
# listener is 127.0.0.1 in every lane, including when the stack is exposed to
# the LAN. So the mapping must NOT interpolate BIND_ADDR.
if grep -qE '^\s*-\s*"127\.0\.0\.1:2112:2112"' "$COMPOSE"; then
    ok "the ops port is published on 127.0.0.1 (D14)"
else
    fail "the ops port mapping is not the literal 127.0.0.1:2112:2112 (D14)"
fi
if grep -E '2112' "$COMPOSE" | grep -q 'BIND_ADDR'; then
    fail "the ops port mapping interpolates BIND_ADDR — it must never follow LAN exposure"
else
    ok "the ops port does not follow BIND_ADDR"
fi

# The same D14 rule for the dev control plane, with more teeth: its static
# token is cluster-admin under AlwaysAllow, so the apiserver port must stay
# a loopback literal whatever BIND_ADDR says.
if grep -qE '^\s*-\s*"127\.0\.0\.1:\$\{KUBE_API_PORT:-6445\}:6443"' "$COMPOSE"; then
    ok "the dev apiserver is published on 127.0.0.1 only (§4.8)"
else
    fail "the dev apiserver mapping is not the literal 127.0.0.1:\${KUBE_API_PORT:-6445}:6443"
fi
if grep -E '6443' "$COMPOSE" | grep -q 'BIND_ADDR'; then
    fail "the dev apiserver mapping interpolates BIND_ADDR — it holds cluster-admin for anyone who connects"
else
    ok "the dev apiserver does not follow BIND_ADDR"
fi

# D12: the boundary is stated in the artifact, because the moment a compose
# file looks deployable somebody deploys it.
if grep -qi 'NOT A DEPLOYMENT PATH' "$COMPOSE"; then
    ok "the header states that this is not a deployment path (D12)"
else
    fail "the header no longer states D12"
fi

# D15: a contributor runs the stack to see THEIR change.
if grep -v '^[[:space:]]*#' "$COMPOSE" | grep -q 'ghcr.io/tuhis'; then
    fail "the stack pulls a released image instead of building this checkout (D15)"
else
    ok "every image is built from this checkout (D15)"
fi

echo "profiles"

# The default `up` is the whole product, moderation portal included — an
# owner decision (2026-08-27, docs/41 §4.8) that supersedes the original
# smallest-default reading of D11: the new developer's first `up` must not
# need a profile to see everything. Still ASSERTED as an exact set, so a
# service cannot join or leave the default lane unnoticed.
default_services=$(dc config --services 2>/dev/null | sort | tr '\n' ' ')
want="admin admin-migrate admin-pg app config-gen fakeidp kine kube-apiserver kube-bootstrap kube-gen relay "
if [ "$default_services" = "$want" ]; then
    ok "a bare \`up\` starts exactly the documented default set (§4.8)"
else
    fail "a bare \`up\` starts: $default_services(want: $want)"
fi

for p in sim rooms telemetry app-dev tls; do
    case "$(dc config --profiles 2>/dev/null)" in
        *"$p"*) ok "the $p profile exists" ;;
        *)      fail "the $p profile is missing" ;;
    esac
done

in_profile() { # in_profile <profile> <service>
    [ "$(COMPOSE_PROFILES=$1 dc config --services 2>/dev/null | grep -c "^$2\$")" = 1 ]
}

check "the tls profile adds caddy"   in_profile tls caddy
check "the sim profile adds pubsim"  in_profile sim pubsim
check "the rooms profile adds pubsim-a" in_profile rooms pubsim-a
check "the rooms profile adds pubsim-b" in_profile rooms pubsim-b
check "the app-dev profile adds it"  in_profile app-dev app-dev

# R42: rooms are on in the dev stack (the relay's own default is off, docs/44
# D17), the static room the `rooms` profile's publishers attach to is defined
# in the file the relay is pointed at, and both publishers name that room.
if dc config 2>/dev/null | grep -q 'GAWK_ROOMS: "1"'; then
    ok "rooms are on by default (GAWK_ROOMS=1)"
else
    fail "rooms are not on by default"
fi
rooms_file=$(dc config 2>/dev/null | sed -n 's/^ *GAWK_ROOMS_FILE: *//p' | tr -d '"')
if grep -q '"code": *"devroom"' "$(dirname "$COMPOSE")/dev/rooms/rooms.json" 2>/dev/null && [ "$rooms_file" = /dev-rooms/rooms.json ]; then
    ok "dev/rooms/rooms.json defines devroom and the relay reads it"
else
    fail "the relay's rooms file and dev/rooms/rooms.json disagree (GAWK_ROOMS_FILE=$rooms_file)"
fi
if [ "$(COMPOSE_PROFILES=rooms dc config 2>/dev/null | grep -c -- '- devroom')" = 2 ]; then
    ok "both rooms-profile publishers attach to devroom"
else
    fail "the rooms-profile publishers do not both attach to devroom"
fi
check "telemetry does not start without its profile" \
    sh -c '! docker compose --env-file /dev/null config --services | grep -q "^telemetry$"'
check "the telemetry profile adds telemetry" in_profile telemetry telemetry

# The read/dashboard listener is never routed publicly (CLAUDE.md), so it is
# not published at all — reach it with `docker compose exec`. Asserted on the
# CONTAINER port: a published mapping renders as `target: 8081`, whatever host
# port it was given.
if COMPOSE_PROFILES=telemetry dc config 2>/dev/null | grep -q 'target: 8081'; then
    fail "the telemetry read listener is published"
else
    ok "the telemetry read listener is not published"
fi

# §5: -insecure belongs to exactly one kind of client — the in-container Go
# publisher on the compose network (`pubsim` and, since R42, the `rooms`
# profile's two copies of it) — and must not spread to any other service.
# Asserted as "one per pubsim* service", so a third copy of the publisher is
# fine and a first use anywhere else is not.
insecure_hits=$(grep -v '^[[:space:]]*#' "$COMPOSE" | grep -c -- '-insecure' || true)
pubsim_services=$(COMPOSE_PROFILES=sim,rooms dc config --services 2>/dev/null | grep -c '^pubsim' || true)
if [ "$insecure_hits" -le "$pubsim_services" ]; then
    ok "-insecure appears only in the pubsim services ($insecure_hits of $pubsim_services)"
else
    fail "-insecure appears $insecure_hits times outside comments for $pubsim_services pubsim services"
fi

# app-dev replaces the built frontend rather than running beside it; compose
# cannot express "this profile switches that service off", so the invocation
# has to be written down where the profile is defined.
if grep -q -- '--scale app=0' "$COMPOSE"; then
    ok "the app-dev profile documents --scale app=0"
else
    fail "the app-dev profile no longer says how to keep the built app out of the way"
fi

echo "gitignore"

# §5: a private key in a public repository is not a mistake with a warning
# attached — CAs must revoke keys reported as compromised within 24 hours.
# dev/generated/kubeconfig carries the control plane's static token: dev-only
# and loopback-scoped, but a credential is a credential.
for p in certs/cert.pem certs/key.pem dev/generated/config.js dev/generated/kubeconfig .env; do
    if git check-ignore -q "$p"; then ok "$p is gitignored"; else fail "$p is NOT gitignored"; fi
done
for p in certs/.gitkeep dev/generated/.gitkeep; do
    if git check-ignore -q "$p"; then fail "$p is ignored (the directory would vanish)"; else ok "$p is tracked"; fi
done

echo
if [ "$failures" -eq 0 ]; then
    echo "dev/stack_test.sh: all checks passed"
else
    echo "dev/stack_test.sh: $failures check(s) failed" >&2
    exit 1
fi
