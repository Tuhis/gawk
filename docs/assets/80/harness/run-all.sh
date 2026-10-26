#!/usr/bin/env bash
# R79 MV0: the full measurement as recorded in docs/assets/80/RESULTS.md.
# Run from the repo root:  bash docs/assets/80/harness/run-all.sh
#
# Needs: node, go, docker, a `pulseaudio` binary (PULSEAUDIO=/path overrides;
# on NixOS: PULSEAUDIO=$(nix build nixpkgs#pulseaudio --no-link --print-out-paths)/bin/pulseaudio),
# and `(cd e2e && npm ci && npx playwright-core install chromium firefox)`.
#
# Chromium and Firefox run on the host; WebKit runs in the Playwright image
# (its Linux build wants Ubuntu 24.04 libraries). All three share one private
# PulseAudio daemon with a null sink: headless Firefox's AudioContext never
# leaves 'suspended' without an output device, and the null sink gives every
# browser the same kind of (fake) device. There is no speaker and no mic.
set -euo pipefail
REPO=$(pwd)
SECONDS_PER_RUN=${SECONDS_PER_RUN:-6}
REPS=${REPS:-2}
OUT=${OUT:-$REPO/docs/assets/80/results}
mkdir -p "$OUT"
BIN=$(mktemp -d)
RT=$(mktemp -d)
chmod 700 "$RT"
PULSEAUDIO=${PULSEAUDIO:-pulseaudio}
PA_LIB=$(dirname "$(dirname "$(readlink -f "$(command -v "$PULSEAUDIO")")")")/lib

(cd gawk-server && CGO_ENABLED=0 go build -o "$BIN/gawk-server" ./cmd/gawk-server)

HOME=$RT XDG_RUNTIME_DIR=$RT "$PULSEAUDIO" -D -n --exit-idle-time=-1 \
  --load=module-native-protocol-unix \
  --load="module-null-sink sink_name=mv0" --load="module-null-source source_name=mv0src"
trap 'HOME=$RT XDG_RUNTIME_DIR=$RT "$PULSEAUDIO" -k || true' EXIT
export XDG_RUNTIME_DIR=$RT PULSE_SERVER=unix:$RT/pulse/native

# NixOS only: Playwright's prebuilt browsers find their libraries through
# nix-ld; its host check uses ldd and does not, so skip the check.
if [ -n "${NIX_LD_LIBRARY_PATH:-}" ]; then
  export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
  export LD_LIBRARY_PATH=$NIX_LD_LIBRARY_PATH:$PA_LIB
fi

node docs/assets/80/harness/run.mjs --browsers chromium,firefox \
  --seconds "$SECONDS_PER_RUN" --reps "$REPS" --out "$OUT" --relay-bin "$BIN/gawk-server"

docker run --rm --network host --ipc=host --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -e PULSE_SERVER=unix:/pulse/native -v "$RT/pulse:/pulse" \
  -v "$REPO:/repo" -v "$BIN:/mv0-bin:ro" -v "$OUT:/out" -w /repo \
  mcr.microsoft.com/playwright:v1.62.1-noble \
  node docs/assets/80/harness/run.mjs --browsers webkit \
  --seconds "$SECONDS_PER_RUN" --reps "$REPS" --out /out --relay-bin /mv0-bin/gawk-server

node docs/assets/80/harness/report.mjs --results "$OUT"
