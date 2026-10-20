#!/usr/bin/env bash
# Builds the uplink lab (docs/75 §4) into $UPLINK_OUT/bin: the Go receiver and
# proxy, the real relay and gawk-loadgen (for F7's runs) and the Rust sender;
# and the browser sender's page into $UPLINK_OUT/chrome-sender.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
mkdir -p "$OUT/bin"
(cd "$ROOT/gawk-server" && for c in gawk-uplinklab gawk-server gawk-loadgen gawk-devcert; do
  go build -o "$OUT/bin/$c" "./cmd/$c"
done)
# D7: the receiver and the relay by the depth of quic-go's per-session datagram
# queue (http3 streamDatagramQueueLen). The tree builds against the vendored
# quic-go at D7's 512 (gawk-server/third_party/quic-go); §11 measured stock
# quic-go's 32 as "today", 4096 for the matrix's "-bigq" builds and 32 / 128 /
# 512 / 4096 for the relay's depth sweep (queue.sh, table N). Each is a copy of
# the vendored source with the constant rewritten, built through its own
# modfile, so the repository's go.mod is never touched. The lab's "today"
# defaults (run.sh's receiver, relay.sh's relay) are the -q32 builds, so §11's
# commands keep measuring what they measured; gawk-server and gawk-server-q512
# are the shipping depth.
(cd "$ROOT/gawk-server"
 src=$(go list -m -f '{{.Dir}}' github.com/quic-go/quic-go)
 for depth in 32 128 512 4096; do
   q="$OUT/quic-go-q$depth"
   rm -rf "$q" && cp -r "$src" "$q" && chmod -R u+w "$q"
   sed -i "s/^const streamDatagramQueueLen = [0-9]*\$/const streamDatagramQueueLen = $depth/" \
     "$q/http3/state_tracking_stream.go"
   grep -q "^const streamDatagramQueueLen = $depth\$" "$q/http3/state_tracking_stream.go" ||
     { echo "uplinklab: $src has no streamDatagramQueueLen constant to rewrite" >&2; exit 1; }
   cp go.mod "$OUT/q$depth.mod" && cp go.sum "$OUT/q$depth.sum"
   go mod edit -modfile "$OUT/q$depth.mod" -replace "github.com/quic-go/quic-go=$q"
   go build -modfile "$OUT/q$depth.mod" -o "$OUT/bin/gawk-server-q$depth" ./cmd/gawk-server
 done
 go build -modfile "$OUT/q32.mod" -o "$OUT/bin/gawk-uplinklab-q32" ./cmd/gawk-uplinklab
 go build -modfile "$OUT/q4096.mod" -o "$OUT/bin/gawk-uplinklab-bigq" ./cmd/gawk-uplinklab
 cp "$OUT/bin/gawk-server-q4096" "$OUT/bin/gawk-server-bigq")
# docs/75 OD5: the browser sender's page (browser/sender.mjs serves it to a
# headless Chrome), built from gawk-app's shipping transport modules.
(cd "$ROOT/gawk-app"
 [ -d node_modules ] || npm ci --no-audit --no-fund
 UPLINK_SENDER_OUT="$OUT/chrome-sender" npx vite build --config lab/uplink-sender.vite.config.ts --logLevel warn)
# The sender is only faithful while it runs the engine's exact QUIC stack, so
# it has no lockfile of its own: it builds from a copy of the desktop
# workspace's (git-ignored), which cargo only trims and extends; then the stack
# it resolved is checked against the workspace's.
LAB="$ROOT/gawk-broadcast-desktop/tools/uplinklab"
cp "$ROOT/gawk-broadcast-desktop/Cargo.lock" "$LAB/Cargo.lock"
cargo build --release --manifest-path "$LAB/Cargo.toml" --target-dir "$OUT/target"
lockver() { awk -v n="$2" '$0 == "name = \"" n "\"" {getline; print; exit}' "$1"; }
for crate in quinn quinn-proto quinn-udp wtransport; do
  ws=$(lockver "$ROOT/gawk-broadcast-desktop/Cargo.lock" "$crate")
  lab=$(lockver "$LAB/Cargo.lock" "$crate")
  if [ "$ws" != "$lab" ]; then
    echo "uplinklab: the sender resolved $crate $lab but the workspace pins $ws" >&2
    exit 1
  fi
done
cp "$OUT/target/release/gawk-uplinklab" "$OUT/bin/sender"
echo "built into $OUT/bin"
