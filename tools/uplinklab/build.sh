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
