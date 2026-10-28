#!/usr/bin/env bash
# R80 NS1 (docs/81 D2, D3): build RNNoise 0.2 to the standalone wasm the
# voice capture worklet runs. The output is committed; CI rebuilds it and
# fails on any byte difference, so never hand-edit rnnoise.wasm.
#
#   bash gawk-app/rnnoise/build.sh            # rewrite gawk-app/rnnoise/rnnoise.wasm
#   bash gawk-app/rnnoise/build.sh OUT_DIR    # build into OUT_DIR instead
#
# Needs docker and the network. Cut down from NS0's
# docs/assets/81/harness/build-wasm.sh to the one variant D1/D3 chose.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(realpath -m "${1:-$HERE}")
# By digest: a byte-compare is exactly where a re-pushed tag would show up.
EMSDK_IMAGE=emscripten/emsdk:4.0.10@sha256:90b757eb11fa9a0e3ce4d2d9f76d932a56018e4accc37b5a28b2783751e60eb7
# The RNNoise 0.2 release tarball (bundled model 0b50c45).
RNNOISE_URL=https://github.com/xiph/rnnoise/releases/download/v0.2/rnnoise-0.2.tar.gz
RNNOISE_SHA256=90fce4b00b9ff24c08dbfe31b82ffd43bae383d85c5535676d28b0a2b11c0d37

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

curl -sSfL -o "$WORK/rnnoise.tar.gz" "$RNNOISE_URL"
echo "$RNNOISE_SHA256  $WORK/rnnoise.tar.gz" | sha256sum -c - >/dev/null
mkdir "$WORK/src" && tar xzf "$WORK/rnnoise.tar.gz" -C "$WORK/src" --strip-components=1

cat >"$WORK/src/gawk-build.sh" <<'INNER'
set -eu
cd /w/src
# No JS glue, no imports: a module the AudioWorklet instantiates itself.
# 0.2's scalar path does not compile (xiph/rnnoise#222), and every engine
# gawk supports has wasm SIMD, so -mavx is the only build (docs/81 D3).
mkdir -p /out
emcc -O3 -flto -msimd128 -mavx -I/w/include -I/w/src -DDISABLE_DEBUG_FLOAT \
  denoise.c rnn.c pitch.c kiss_fft.c celt_lpc.c nnet.c nnet_default.c \
  parse_lpcnet_weights.c rnnoise_data.c rnnoise_tables.c \
  -sSTANDALONE_WASM --no-entry \
  -sEXPORTED_FUNCTIONS='["_rnnoise_create","_rnnoise_destroy","_rnnoise_process_frame","_malloc","_free"]' \
  -sFILESYSTEM=0 -sINITIAL_MEMORY=4MB -sALLOW_MEMORY_GROWTH=0 -sSTACK_SIZE=256KB \
  -o /out/rnnoise.wasm
INNER
# Sources in and the wasm out through the container's stdin and stdout, not
# bind mounts: CI's runners talk to a docker daemon that need not share
# their filesystem. emcc's chatter goes to stderr.
mkdir "$WORK/out"
tar cf - -C "$WORK/src" . |
  docker run --rm -i --entrypoint sh "$EMSDK_IMAGE" -c \
    'mkdir /w && tar xf - -C /w && sh /w/gawk-build.sh >&2 && tar cf - -C /out rnnoise.wasm' |
  tar xf - -C "$WORK/out"
cp "$WORK/out/rnnoise.wasm" "$OUT/rnnoise.wasm"
chmod 644 "$OUT/rnnoise.wasm"
cp "$WORK/src/COPYING" "$OUT/COPYING"
sha256sum "$OUT/rnnoise.wasm" | sed "s|  .*/|  |"
