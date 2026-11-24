#!/usr/bin/env bash
# Builds the deep-buffer throughput lab (R87 SG0, docs/91 §4) into
# $DVRLAB_OUT: static relay, publisher and viewer binaries for the lab's
# Alpine containers, a throwaway CA with a relay certificate (Firefox refuses
# a self-signed relay even with certificate hashes, Mozilla bug 1873263), and
# the Firefox enterprise policy that trusts that CA.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${DVRLAB_OUT:-$ROOT/tools/dvrlab/out}
mkdir -p "$OUT/bin" "$OUT/pki"
(cd "$ROOT/gawk-server" && for c in gawk-server gawk-pubsim gawk-loadgen; do
  CGO_ENABLED=0 go build -o "$OUT/bin/$c" "./cmd/$c"
done)
docker image inspect dvrlab-alpine >/dev/null 2>&1 || docker build -q -t dvrlab-alpine - >/dev/null <<'DOCKERFILE'
FROM alpine:3.20
RUN apk add --no-cache iproute2 curl openssl
DOCKERFILE
docker run --rm -v "$OUT/pki:/pki" -w /pki dvrlab-alpine sh -c '
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout ca.key -out ca.pem -days 7 \
    -subj "/CN=dvrlab CA" -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout relay.key -out relay.csr -subj "/CN=dvrlab-relay" 2>/dev/null
  printf "subjectAltName=DNS:dvrlab-relay\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n" > ext.cnf
  openssl x509 -req -in relay.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out relay.pem -days 7 -extfile ext.cnf 2>/dev/null
  chmod 644 *'
# An NSS store that trusts the CA, for a Firefox profile: Playwright's Firefox
# ignores enterprise policies, so Certificates.Install is no way in.
rm -rf "$OUT/ff-profile" && mkdir -p "$OUT/ff-profile"
docker run --rm -v "$OUT:/o" alpine:3.20 sh -c '
  apk add -q nss-tools >/dev/null
  certutil -N -d sql:/o/ff-profile --empty-password
  certutil -A -d sql:/o/ff-profile -t "C,," -n dvrlab-ca -i /o/pki/ca.pem
  chmod -R a+rwX /o/ff-profile'
docker network inspect dvrlab >/dev/null 2>&1 || docker network create dvrlab >/dev/null
echo "built into $OUT"
