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
# D7's measured fix: the receiver and the relay against copies of the pinned
# quic-go whose per-session datagram queue (http3 streamDatagramQueueLen) holds
# more than 32 — 4096 for the "-bigq" builds the matrix uses, and 128 / 512 /
# 4096 for the relay's depth sweep (queue.sh, §11 table N). Built through
# separate modfiles, so the repository's go.mod is never touched.
(cd "$ROOT/gawk-server"
 qv=$(go list -m -f '{{.Version}}' github.com/quic-go/quic-go)
 src=$(go list -m -f '{{.Dir}}' github.com/quic-go/quic-go)
 for depth in 128 512 4096; do
   q="$OUT/quic-go-q$depth"
   rm -rf "$q" && cp -r "$src" "$q" && chmod -R u+w "$q"
   sed -i "s/^const streamDatagramQueueLen = 32\$/const streamDatagramQueueLen = $depth/" \
     "$q/http3/state_tracking_stream.go"
   grep -q "^const streamDatagramQueueLen = $depth\$" "$q/http3/state_tracking_stream.go" ||
     { echo "uplinklab: quic-go $qv no longer has streamDatagramQueueLen = 32" >&2; exit 1; }
   cp go.mod "$OUT/q$depth.mod" && cp go.sum "$OUT/q$depth.sum"
   go mod edit -modfile "$OUT/q$depth.mod" -replace "github.com/quic-go/quic-go=$q"
   go build -modfile "$OUT/q$depth.mod" -o "$OUT/bin/gawk-server-q$depth" ./cmd/gawk-server
 done
 go build -modfile "$OUT/q4096.mod" -o "$OUT/bin/gawk-uplinklab-bigq" ./cmd/gawk-uplinklab
 cp "$OUT/bin/gawk-server-q4096" "$OUT/bin/gawk-server-bigq")
# Table G's R3 and table N's "drain" row: the stock relay with drain.patch
# (the publisher loop drains quic-go's queue on its own goroutine) applied
# through a build overlay, so the tree is never modified.
(cd "$ROOT/gawk-server"
 mkdir -p "$OUT/drain"
 patch -s -o "$OUT/drain/server.go" internal/transport/server.go "$ROOT/tools/uplinklab/drain.patch"
 printf '{"Replace":{"%s":"%s"}}\n' "$PWD/internal/transport/server.go" "$OUT/drain/server.go" > "$OUT/drain/overlay.json"
 go build -overlay "$OUT/drain/overlay.json" -o "$OUT/bin/gawk-server-drain" ./cmd/gawk-server)
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
