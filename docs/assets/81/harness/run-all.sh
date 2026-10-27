#!/usr/bin/env bash
# R80 NS0: the full measurement as recorded in docs/assets/81/RESULTS.md.
# Run from the repo root:  bash docs/assets/81/harness/run-all.sh WORK_DIR
#
# Needs: node, docker, ffmpeg, gcc, a `pulseaudio` binary (PULSEAUDIO=/path
# overrides; on NixOS: PULSEAUDIO=$(nix build nixpkgs#pulseaudio --no-link
# --print-out-paths)/bin/pulseaudio), and
# `(cd e2e && npm ci && npx playwright-core install chromium firefox)`.
# WORK_DIR receives the wasm builds, the corpus and the Chromium captures;
# only docs/assets/81/results/*.json are kept in the repository.
#
# Chromium and Firefox run on the host, WebKit in the Playwright image, all
# on one private PulseAudio null sink (as docs/assets/80 does: headless
# Firefox's AudioContext never leaves 'suspended' without an output device).
# The offline run goes last so the browsers' CPU numbers are not contended.
set -euo pipefail
REPO=$(pwd)
WORK=$(realpath -m "${1:?usage: run-all.sh WORK_DIR}")
H=$REPO/docs/assets/81/harness
OUT=$REPO/docs/assets/81/results
mkdir -p "$WORK" "$OUT"

[ -f "$WORK/wasm/main-avx2.wasm" ] || bash "$H/build-wasm.sh" "$WORK/wasm"
[ -f "$WORK/corpus/speech-a.f32" ] || bash "$H/fetch-corpus.sh" "$WORK/corpus"

# The native reference: main's sources at the pinned commit, built on the host.
SRC=$WORK/src-main
if [ ! -x "$WORK/native-ref" ]; then
  rm -rf "$SRC"
  git clone -q https://github.com/xiph/rnnoise "$SRC"
  git -C "$SRC" checkout -q 70f1d256acd4b34a572f999a05c87bf00b67730d
  curl -sSfL "https://media.xiph.org/rnnoise/models/rnnoise_data-$(cat "$SRC/model_version").tar.gz" | tar xzom -C "$SRC" src/
  (cd "$SRC/src" && gcc -O2 -DDISABLE_DEBUG_FLOAT -I../include -I. "$H/native-ref.c" denoise.c rnn.c pitch.c \
    kiss_fft.c celt_lpc.c nnet.c nnet_default.c parse_lpcnet_weights.c rnnoise_data.c rnnoise_tables.c -lm -o "$WORK/native-ref")
fi

# Keep it in /tmp: PulseAudio's socket path has to fit a Unix socket's
# 108 bytes; with a longer one PulseAudio never listens and headless
# Firefox's audio probes hang.
RT=$(mktemp -d)
chmod 700 "$RT"
PULSEAUDIO=${PULSEAUDIO:-pulseaudio}
PA_LIB=$(dirname "$(dirname "$(readlink -f "$(command -v "$PULSEAUDIO")")")")/lib
HOME=$RT XDG_RUNTIME_DIR=$RT "$PULSEAUDIO" -D -n --exit-idle-time=-1 \
  --load=module-native-protocol-unix \
  --load="module-null-sink sink_name=ns0" --load="module-null-source source_name=ns0src"
trap 'HOME=$RT XDG_RUNTIME_DIR=$RT "$PULSEAUDIO" -k || true' EXIT
export XDG_RUNTIME_DIR=$RT PULSE_SERVER=unix:$RT/pulse/native
if [ -n "${NIX_LD_LIBRARY_PATH:-}" ]; then
  export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
  export LD_LIBRARY_PATH=$NIX_LD_LIBRARY_PATH:$PA_LIB
fi

node "$H/run-browser.mjs" --browsers chromium,firefox --wasm "$WORK/wasm" \
  --corpus "$WORK/corpus" --out "$OUT" --capture-out "$WORK/capture"

PWV=$(node -e "console.log(require('$REPO/e2e/node_modules/playwright-core/package.json').version)")
docker run --rm --network host --ipc=host --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -e PULSE_SERVER=unix:/pulse/native -v "$RT/pulse:/pulse" \
  -v "$REPO:/repo" -v "$WORK/wasm:/wasm:ro" -w /repo \
  "mcr.microsoft.com/playwright:v$PWV-noble" \
  node docs/assets/81/harness/run-browser.mjs --browsers webkit --wasm /wasm --out /repo/docs/assets/81/results

node "$H/offline.mjs" --wasm "$WORK/wasm" --corpus "$WORK/corpus" --native "$WORK/native-ref" --out "$OUT/offline.json"
node "$H/capture.mjs" --wasm "$WORK/wasm" --capture "$WORK/capture" --out "$OUT/capture.json"
node "$H/resample.mjs" --wasm "$WORK/wasm" --corpus "$WORK/corpus" --out "$OUT/resample.json"
node "$H/report.mjs" --results "$OUT"
