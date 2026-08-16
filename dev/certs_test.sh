#!/bin/sh
# Tests for dev/certs.sh (R38, docs/41 LD4). Every row of the §4.6 failure
# register has a case here — none of them may be silently unimplemented — plus
# the two properties that matter most and are invisible at a glance: that the
# ACME lane NEVER submits before the challenge record is live, and that a DNS
# provider credential never reaches stdout, stderr or a log.
#
# Everything runs against stub `dig`, `mkcert` and `docker` binaries on PATH,
# inside a throwaway copy of the repository skeleton — no network, no CA, and
# nothing written to the real ./certs or ./.env. Run from the repo root:
#
#     ./dev/certs_test.sh
set -eu

REPO=$(cd "$(dirname "$0")/.." && pwd)
PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
want() { # want <description> <needle> <file>
    if grep -qF -e "$2" "$3"; then
        ok "$1"
    else
        bad "$1 (no '$2' in output)"
        # Enough context to see what happened instead, not the whole file —
        # .env alone is 100 lines of comments.
        grep -v '^[[:space:]]*#' "$3" | grep -v '^$' | tail -15 | sed 's/^/        /'
    fi
}
want_not() { # want_not <description> <needle> <file>
    if grep -qF -e "$2" "$3"; then bad "$1 ('$2' WAS in output)"; else ok "$1"; fi
}

# ── a throwaway stack ─────────────────────────────────────────────────────

setup() { # setup → sets T
    # `VAR=x run_certs …` persists past the call for a shell FUNCTION, so the
    # stub knobs are cleared per test rather than per invocation — otherwise a
    # rate-limit case quietly rigs every case after it.
    unset DNS_TXT_AFTER STUB_LEGO STUB_PORT_BUSY 2>/dev/null || true
    T=$(mktemp -d)
    mkdir -p "$T/dev" "$T/certs" "$T/bin"
    cp "$REPO/dev/certs.sh" "$T/dev/certs.sh"
    cp "$REPO/.env.example" "$T/.env.example"
    : > "$T/order.log"
    : > "$T/docker.log"
    : > "$T/dig.log"
    write_stub_dig
    write_stub_docker
    write_stub_mkcert
}

teardown() { rm -rf "$T"; }

# Every run is watchdogged: this suite drives a wizard that waits on DNS and
# on a subprocess's stdin, and a test that hangs is worse than one that fails.
run_certs() { # run_certs <args…> → output in $T/out
    (
        cd "$T"
        PATH="$T/bin:$PATH"
        export PATH T
        export GAWK_CERTS_NONINTERACTIVE=1
        export ACME_TXT_POLL=1 ACME_TXT_TIMEOUT=20
        export DNS_TXT_AFTER="${DNS_TXT_AFTER:-}" STUB_LEGO="${STUB_LEGO:-}" STUB_PORT_BUSY="${STUB_PORT_BUSY:-}"
        sh dev/certs.sh "$@"
    ) > "$T/out" 2>&1 &
    _pid=$!
    _waited=0
    while kill -0 "$_pid" 2>/dev/null; do
        if [ "$_waited" -ge "${RUN_TIMEOUT:-60}" ]; then
            kill -9 "$_pid" 2>/dev/null || true
            pkill -9 -f "sh dev/certs.sh" 2>/dev/null || true
            echo "[timed out after ${_waited}s]" >> "$T/out"
            break
        fi
        sleep 1
        _waited=$((_waited + 1))
    done
    wait "$_pid" 2>/dev/null || echo "[exit $?]" >> "$T/out"
}

# A resolver whose answers are files: $T/dns/<type>.<name> holds what it
# returns, and DNS_TXT_AFTER makes the challenge record appear only after N
# queries — the propagation delay this lane exists to survive.
write_stub_dig() {
    cat > "$T/bin/dig" <<'STUB'
#!/bin/sh
type=; name=; server=
for a in "$@"; do
  case "$a" in
    +*) ;;
    @*) server=${a#@} ;;
    A|TXT|CAA|NS) type=$a ;;
    *) [ -z "$name" ] && name=$a ;;
  esac
done
echo "$type $name ${server:-default}" >> "$T/dig.log"
file="$T/dns/$type.$name"
[ -n "$server" ] && [ -f "$T/dns/$type.$name@$server" ] && file="$T/dns/$type.$name@$server"
if [ "$type" = TXT ] && [ -n "${DNS_TXT_AFTER:-}" ]; then
  n=$(grep -c "^TXT " "$T/dig.log")
  if [ "$n" -lt "$DNS_TXT_AFTER" ]; then exit 0; fi
  grep -q TXT-VISIBLE "$T/order.log" || echo TXT-VISIBLE >> "$T/order.log"
fi
[ -f "$file" ] && cat "$file"
exit 0
STUB
    chmod +x "$T/bin/dig"
    mkdir -p "$T/dns"
}

# Stands in for both `docker compose ps` (never running) and `docker run`
# (the port probe, and lego). STUB_LEGO decides which lego outcome it plays.
write_stub_docker() {
    cat > "$T/bin/docker" <<'STUB'
#!/bin/sh
echo "$*" >> "$T/docker.log"
case "$1" in
  compose) exit 0 ;;   # `compose ps` → nothing running
esac
# Is this the port probe or lego?
case "$*" in
  *goacme/lego*) ;;
  *) exit "${STUB_PORT_BUSY:-0}" ;;
esac
echo LEGO-STARTED >> "$T/order.log"
case "${STUB_LEGO:-manual}" in
  ratelimit-failed)
    echo "acme: error: 429 :: urn:ietf:params:acme:error:rateLimited :: Error creating new order :: too many failed authorizations recently"
    exit 1 ;;
  ratelimit-issued)
    echo "acme: error: 429 :: too many certificates already issued for example.com"
    exit 1 ;;
  api)
    echo "[INFO] [*.dev.example.com] acme: Obtaining bundled SAN certificate"
    ;;
  manual)
    echo "lego: Please create the following TXT record in your example.com. zone:"
    echo 'lego: _acme-challenge.dev.example.com. 120 IN TXT "TESTCHALLENGEVALUE0001"'
    echo "lego: Press 'Enter' when you have added the TXT record to proceed."
    # Blocks until the wizard says the record is live. That moment is the
    # assertion this whole stub exists for.
    read -r _line || true
    echo PROCEED >> "$T/order.log"
    ;;
esac
mkdir -p "$T/certs/lego/certificates"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$T/certs/lego/certificates/_.dev.example.com.key" \
  -out "$T/certs/lego/certificates/_.dev.example.com.crt" \
  -days 90 -subj "/CN=stub" -addext "subjectAltName=DNS:*.dev.example.com" 2>/dev/null
exit 0
STUB
    chmod +x "$T/bin/docker"
}

write_stub_mkcert() {
    cat > "$T/bin/mkcert" <<'STUB'
#!/bin/sh
echo "mkcert $*" >> "$T/docker.log"
case "$1" in
  -install) echo "The local CA is already installed"; exit 0 ;;
  -CAROOT)  echo "$T/caroot"; exit 0 ;;
esac
cert=; key=; names=
while [ $# -gt 0 ]; do
  case "$1" in
    -cert-file) cert=$2; shift 2 ;;
    -key-file)  key=$2; shift 2 ;;
    *) names="$names $1"; shift ;;
  esac
done
san=
for n in $names; do
  case "$n" in
    ::1)    item="IP:::1" ;;
    [0-9]*) item="IP:$n" ;;
    *)      item="DNS:$n" ;;
  esac
  san="${san:+$san,}$item"
done
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$key" -out "$cert" -days 30 -subj "/CN=mkcert-stub" \
  -addext "subjectAltName=$san" 2>/dev/null
STUB
    chmod +x "$T/bin/mkcert"
}

# A self-signed certificate with chosen SANs and lifetime, for the checks that
# read one off disk.
make_cert() { # make_cert <days> <san-list>
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$T/certs/key.pem" -out "$T/certs/cert.pem" \
        -days "$1" -subj "/CN=test" -addext "subjectAltName=$2" 2>/dev/null
    chmod 600 "$T/certs/key.pem"
}

env_line() { grep "^$1=" "$T/.env" | tail -1; }

# ── the default lane ──────────────────────────────────────────────────────

echo "devcert lane"
setup
run_certs devcert
want "writes .env from the example"                 "CERT_MODE=devcert" "$T/.env"
want "keeps the stack on this machine by default"   "BIND_ADDR=127.0.0.1" "$T/.env"
want "points the app at the relay over IPv4"        "RELAY_URL=https://127.0.0.1:4433" "$T/.env"
want "records the host uid so the pair stays yours" "HOST_UID=" "$T/.env"
want "leaves the tls profile off"                   "COMPOSE_PROFILES=" "$T/.env"
teardown

# ── §4.6: the certificate does not cover GAWK_HOST ────────────────────────

echo
echo "certificate checks (§4.6)"
setup
run_certs devcert                     # writes .env with GAWK_HOST=localhost
make_cert 30 "DNS:gawk.example.com"   # …and a certificate for something else
run_certs check
want "a SAN mismatch refuses rather than failing in the browser" "does NOT cover localhost" "$T/out"
teardown

setup
run_certs devcert
make_cert 30 "DNS:localhost,IP:127.0.0.1"
run_certs check
want "a covering certificate passes" "certificate covers localhost" "$T/out"
want_not "…and says nothing about SANs" "does NOT cover" "$T/out"
teardown

# Near expiry: the trusted lanes warn at 30 days, so a 30-day certificate in
# mkcert mode is inside the window and a 90-day one is not.
setup
run_certs devcert
make_cert 20 "DNS:localhost"
sed -i.bak 's/^CERT_MODE=.*/CERT_MODE=mkcert/' "$T/.env"
run_certs check
want "warns when a trusted-lane certificate is inside 30 days" "expires within 30 days" "$T/out"
teardown

setup
run_certs devcert
make_cert 90 "DNS:localhost"
sed -i.bak 's/^CERT_MODE=.*/CERT_MODE=mkcert/' "$T/.env"
run_certs check
want_not "…and not when it is comfortably ahead" "expires within" "$T/out"
teardown

# Clock skew / expired: a certificate outside its own validity window is
# otherwise inscrutable, so the check names the clock.
setup
run_certs devcert
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$T/certs/key.pem" -out "$T/certs/cert.pem" \
    -not_before 20200101000000Z -not_after 20200102000000Z \
    -subj "/CN=test" -addext "subjectAltName=DNS:localhost" 2>/dev/null \
    || make_cert 1 "DNS:localhost"   # older openssl: fall back, see below
if openssl x509 -in "$T/certs/cert.pem" -noout -checkend 0 >/dev/null 2>&1; then
    ok "clock-skew check skipped (this openssl cannot backdate a certificate; reproduce by setting the clock forward)"
else
    run_certs check
    want "names the clock when the certificate is outside its validity window" "check this machine's clock" "$T/out"
fi
teardown

# ── §4.6: port already in use ─────────────────────────────────────────────

echo
echo "port checks (§4.6)"
setup
run_certs devcert
STUB_PORT_BUSY=1 run_certs check
want "a busy port names the port" "8080/tcp" "$T/out"
want "…and the .env variable that changes it" "change APP_HTTP_ADDR in .env" "$T/out"
teardown

# ── §4.6: mkcert absent ───────────────────────────────────────────────────

echo
echo "mkcert lane (§4.3)"
setup
rm "$T/bin/mkcert"
# An empty PATH but for the stubs, so the real mkcert (if the developer has
# one) cannot make this pass by accident.
(
    cd "$T"
    PATH="$T/bin:/usr/bin:/bin"
    export PATH T
    GAWK_CERTS_NONINTERACTIVE=1 sh dev/certs.sh mkcert
) > "$T/out" 2>&1 || true
want "a missing mkcert prints the install line" "mkcert is not on PATH" "$T/out"
want_not "…and does not install it" "installing the local CA" "$T/out"
teardown

setup
run_certs mkcert
want "issues for the pretty name"      "gawk.localhost" "$T/docker.log"
want "…and for loopback"               "127.0.0.1" "$T/docker.log"
want "switches the stack to HTTPS"     "APP_ORIGIN=https://gawk.localhost:8443" "$T/.env"
want "turns the tls front door on"     "COMPOSE_PROFILES=tls" "$T/.env"
want "stops the relay self-signing"    "DEV_CERT=" "$T/.env"
want "keeps the relay on an IPv4 literal" "RELAY_URL=https://127.0.0.1:4433" "$T/.env"
want "says other devices need the root" "rootCA.pem" "$T/out"
teardown

# ── §4.6: CAA, propagation, rate limits (the ACME lane) ───────────────────

echo
echo "ACME lane (§4.4, §4.6)"

acme_env() { # acme_env [extra .env lines…]
    run_certs devcert
    {
        echo "ACME_DOMAIN=example.com"
        echo "ACME_SUBDOMAIN=dev"
        echo "ACME_EMAIL=dev@example.com"
        echo "ACME_STAGING=1"
    } >> "$T/.env"
    echo "ns1.example.com." > "$T/dns/NS.example.com"
    echo 'TESTCHALLENGEVALUE0001' > "$T/dns/TXT._acme-challenge.dev.example.com"
    echo "127.0.0.1" > "$T/dns/A.gawk.dev.example.com"
}

setup
acme_env
echo '0 issue "digicert.com"' > "$T/dns/CAA.example.com"
run_certs acme
want "a forbidding CAA record is named"        "CAA on example.com forbids" "$T/out"
want_not "…and no issuance is spent"           "LEGO-STARTED" "$T/order.log"
teardown

setup
acme_env
# The record only becomes visible on the fourth query — the wizard must sit
# through the first three rather than submitting.
DNS_TXT_AFTER=4 run_certs acme
want "the wizard waits for propagation"        "still waiting" "$T/out"
if [ "$(head -2 "$T/order.log" | tail -1)" = "TXT-VISIBLE" ] && grep -q PROCEED "$T/order.log"; then
    ok "it never submits before the record is live on the authoritative NS"
else
    bad "submission order was: $(tr '\n' ' ' < "$T/order.log")"
fi
want "it asks the authoritative nameserver, not the default resolver" "TXT _acme-challenge.dev.example.com ns1.example.com" "$T/dig.log"
want "the issued pair lands where the stack reads it" "certs/cert.pem" "$T/out"
teardown

setup
acme_env
DNS_TXT_AFTER=200 ACME_TXT_TIMEOUT=3 run_certs acme
want "giving up says nothing was submitted"    "no rate limit was spent" "$T/out"
want_not "…and the certificate is untouched"   "certs/cert.pem, certs/key.pem" "$T/out"
teardown

setup
acme_env
STUB_LEGO=ratelimit-failed run_certs acme
want "a failed-validation rate limit is explained" "5 per hour" "$T/out"
teardown

setup
acme_env
STUB_LEGO=ratelimit-issued run_certs acme
want "an issuance rate limit is explained"     "50 per registered domain per week" "$T/out"
teardown

# DNS rebinding: the name answers authoritatively but not through the local
# resolver, which is what a rebinding-protected resolver looks like.
setup
acme_env
rm -f "$T/dns/A.gawk.dev.example.com"
echo "127.0.0.1" > "$T/dns/A.gawk.dev.example.com@ns1.example.com"
run_certs acme
want "rebinding protection is named"           "DNS-rebinding protection" "$T/out"
want "…with the exact /etc/hosts line"         "127.0.0.1  gawk.dev.example.com" "$T/out"
teardown

# The manual sub-lane cannot renew itself, and says so when it is chosen
# rather than in 90 days when the stack breaks.
setup
acme_env
run_certs acme
want "the manual sub-lane states it cannot renew" "cannot renew automatically" "$T/out"
teardown

# ── secrets ───────────────────────────────────────────────────────────────

echo
echo "secrets (§5)"
setup
acme_env
echo "CLOUDFLARE_DNS_API_TOKEN=s3cr3t-token-value" >> "$T/.env"
echo "LEGO_DNS_PROVIDER=cloudflare" >> "$T/.env"
STUB_LEGO=api run_certs acme
want_not "the provider token never reaches stdout/stderr" "s3cr3t-token-value" "$T/out"
want_not "…and is not in the docker command line either" "s3cr3t-token-value" "$T/docker.log"
want "it is passed through a file instead"     "--env-file" "$T/docker.log"
teardown

echo
if [ "$FAIL" -eq 0 ]; then
    echo "dev/certs_test.sh: $PASS checks passed"
else
    echo "dev/certs_test.sh: $FAIL of $((PASS + FAIL)) checks failed" >&2
    exit 1
fi
