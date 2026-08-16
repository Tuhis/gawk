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

# The default `up` is relay + app + config-gen only: a default that starts
# five containers is a default people stop running (D11).
default_services=$(docker compose config --services 2>/dev/null | sort | tr '\n' ' ')
if [ "$default_services" = "app config-gen relay " ]; then
    ok "a bare \`up\` starts exactly relay, config-gen and app"
else
    fail "a bare \`up\` starts: $default_services"
fi

for p in sim telemetry app-dev tls; do
    case "$(docker compose config --profiles 2>/dev/null)" in
        *"$p"*) ok "the $p profile exists" ;;
        *)      fail "the $p profile is missing" ;;
    esac
done

profiled_services() { COMPOSE_PROFILES=$1 docker compose config --services 2>/dev/null | sort | tr '\n' ' '; }

check "the tls profile adds caddy"          sh -c '[ "$(COMPOSE_PROFILES=tls docker compose config --services | grep -c "^caddy$")" = 1 ]'
check "the sim profile adds pubsim"         sh -c '[ "$(COMPOSE_PROFILES=sim docker compose config --services | grep -c "^pubsim$")" = 1 ]'
check "telemetry does not start without its profile" \
    sh -c '! docker compose config --services | grep -q "^telemetry$"'
check "the telemetry profile adds telemetry" \
    sh -c '[ "$(COMPOSE_PROFILES=telemetry docker compose config --services | grep -c "^telemetry$")" = 1 ]'

# The read/dashboard listener is never routed publicly (CLAUDE.md), so it is
# not published at all — reach it with `docker compose exec`.
if COMPOSE_PROFILES=telemetry docker compose config 2>/dev/null | grep -q 'published: "8081"'; then
    fail "the telemetry read listener is published"
else
    ok "the telemetry read listener is not published"
fi

# §5: -insecure appears exactly once in this milestone — the in-container Go
# client on the compose network — and must not spread.
insecure_hits=$(grep -v '^[[:space:]]*#' "$COMPOSE" | grep -c -- '-insecure' || true)
if [ "$insecure_hits" -le 1 ]; then
    ok "-insecure appears at most once outside comments"
else
    fail "-insecure appears $insecure_hits times outside comments"
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
for p in certs/cert.pem certs/key.pem dev/generated/config.js .env; do
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
