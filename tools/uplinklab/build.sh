#!/usr/bin/env bash
# Builds the uplink lab (docs/73 §4) into $UPLINK_OUT/bin: the Go receiver and
# proxy, the real relay and gawk-loadgen (for F7's runs) and the Rust sender.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
mkdir -p "$OUT/bin"
(cd "$ROOT/gawk-server" && for c in gawk-uplinklab gawk-server gawk-loadgen gawk-devcert; do
  go build -o "$OUT/bin/$c" "./cmd/$c"
done)
# D7's measured fix: the receiver and the relay against a copy of the pinned
# quic-go whose per-session datagram queue (http3 streamDatagramQueueLen) holds
# 4096 instead of 32. Built through a separate modfile, so the repository's
# go.mod is never touched.
(cd "$ROOT/gawk-server"
 qv=$(go list -m -f '{{.Version}}' github.com/quic-go/quic-go)
 src=$(go list -m -f '{{.Dir}}' github.com/quic-go/quic-go)
 rm -rf "$OUT/quic-go-bigq" && cp -r "$src" "$OUT/quic-go-bigq" && chmod -R u+w "$OUT/quic-go-bigq"
 sed -i 's/^const streamDatagramQueueLen = 32$/const streamDatagramQueueLen = 4096/' \
   "$OUT/quic-go-bigq/http3/state_tracking_stream.go"
 grep -q '^const streamDatagramQueueLen = 4096$' "$OUT/quic-go-bigq/http3/state_tracking_stream.go" ||
   { echo "uplinklab: quic-go $qv no longer has streamDatagramQueueLen = 32" >&2; exit 1; }
 cp go.mod "$OUT/bigq.mod" && cp go.sum "$OUT/bigq.sum"
 go mod edit -modfile "$OUT/bigq.mod" -replace "github.com/quic-go/quic-go=$OUT/quic-go-bigq"
 for c in gawk-uplinklab gawk-server; do
   go build -modfile "$OUT/bigq.mod" -o "$OUT/bin/$c-bigq" "./cmd/$c"
 done)
# The sender is only faithful while it runs the engine's exact QUIC stack: its
# own lockfile must pin what the desktop workspace pins.
lockver() { awk -v n="$2" '$0 == "name = \"" n "\"" {getline; print; exit}' "$1"; }
for crate in quinn quinn-proto quinn-udp wtransport; do
  ws=$(lockver "$ROOT/gawk-broadcast-desktop/Cargo.lock" "$crate")
  lab=$(lockver "$ROOT/gawk-broadcast-desktop/tools/uplinklab/Cargo.lock" "$crate")
  if [ "$ws" != "$lab" ]; then
    echo "uplinklab: $crate is $lab in the lab but $ws in the workspace; update tools/uplinklab/Cargo.lock" >&2
    exit 1
  fi
done
cargo build --release --manifest-path "$ROOT/gawk-broadcast-desktop/tools/uplinklab/Cargo.toml" \
  --target-dir "$OUT/target"
cp "$OUT/target/release/gawk-uplinklab" "$OUT/bin/sender"
echo "built into $OUT/bin"
