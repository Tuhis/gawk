#!/usr/bin/env bash
# R80 NS0: build every RNNoise → WebAssembly variant the spike compares
# (docs/81 §12). Run from the repo root:
#   bash docs/assets/81/harness/build-wasm.sh OUT_DIR
#
# Needs docker and the network. Sources are pinned by hash:
#   v02   the RNNoise 0.2 release tarball (model 0b50c45)
#   main    xiph/rnnoise main at MAIN_COMMIT plus the model tarball its
#           model_version names (the git tree does not carry the weights)
#   mumble  the commit Mumble's 3rdparty/rnnoise-src submodule pins
#           (MUMBLE_COMMIT, 2024-08-24) with its model, the same way
# Each is built scalar, -mavx and -mavx2 (Emscripten lowers the SSE/AVX
# intrinsics in vec_avx.h to 128-bit wasm SIMD). The 0.2 tarball's scalar
# path does not compile (it includes an os_support.h it does not ship,
# xiph/rnnoise#222, fixed on main by 372f7b4), so there is no v02-scalar.
set -euo pipefail
OUT=$(realpath -m "${1:?usage: build-wasm.sh OUT_DIR}")
EMSDK_IMAGE=emscripten/emsdk:4.0.10
V02_SHA256=90fce4b00b9ff24c08dbfe31b82ffd43bae383d85c5535676d28b0a2b11c0d37
MAIN_COMMIT=70f1d256acd4b34a572f999a05c87bf00b67730d
MODEL_SHA256=0a8755f8e2d834eff6a54714ecc7d75f9932e845df35f8b59bc52a7cfe6e8b37
MUMBLE_COMMIT=d98345814b0531e61bf10d463be21fa94b352f9c
MUMBLE_MODEL_SHA256=5e7841199cf2947fc32f6eeaf6faffab5c2dd3da69e6a6e4830625a10285a09d

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

curl -sSfL -o "$WORK/v02.tar.gz" https://github.com/xiph/rnnoise/releases/download/v0.2/rnnoise-0.2.tar.gz
echo "$V02_SHA256  $WORK/v02.tar.gz" | sha256sum -c -
mkdir "$WORK/v02" && tar xzf "$WORK/v02.tar.gz" -C "$WORK/v02" --strip-components=1

git clone -q https://github.com/xiph/rnnoise "$WORK/main"
git -C "$WORK/main" checkout -q "$MAIN_COMMIT"
[ "$(cat "$WORK/main/model_version")" = "$MODEL_SHA256" ]
curl -sSfL -o "$WORK/model.tar.gz" "https://media.xiph.org/rnnoise/models/rnnoise_data-$MODEL_SHA256.tar.gz"
echo "$MODEL_SHA256  $WORK/model.tar.gz" | sha256sum -c -
tar xzomf "$WORK/model.tar.gz" -C "$WORK/main" src/

git clone -q "$WORK/main" "$WORK/mumble"
git -C "$WORK/mumble" checkout -q "$MUMBLE_COMMIT"
[ "$(cat "$WORK/mumble/model_version")" = "$MUMBLE_MODEL_SHA256" ]
curl -sSfL -o "$WORK/mumble-model.tar.gz" "https://media.xiph.org/rnnoise/models/rnnoise_data-$MUMBLE_MODEL_SHA256.tar.gz"
echo "$MUMBLE_MODEL_SHA256  $WORK/mumble-model.tar.gz" | sha256sum -c -
tar xzomf "$WORK/mumble-model.tar.gz" -C "$WORK/mumble" src/

cat >"$WORK/inner.sh" <<'EOF'
set -eu
# No JS glue, no imports: a module the AudioWorklet can instantiate itself.
EXP='["_rnnoise_create","_rnnoise_destroy","_rnnoise_process_frame","_rnnoise_get_frame_size","_malloc","_free"]'
build() { name=$1; root=$2; data=$3; shift 3
  cd "$root/src"
  # DISABLE_DEBUG_FLOAT drops main's float copies of the int8 weights
  # (78 MB of C source, unused at inference time).
  emcc -O3 -flto -I"$root/include" -I"$root/src" -DDISABLE_DEBUG_FLOAT "$@" \
    denoise.c rnn.c pitch.c kiss_fft.c celt_lpc.c nnet.c nnet_default.c \
    parse_lpcnet_weights.c "$data" rnnoise_tables.c \
    -sSTANDALONE_WASM --no-entry -sEXPORTED_FUNCTIONS="$EXP" -sFILESYSTEM=0 \
    -sINITIAL_MEMORY=4MB -sALLOW_MEMORY_GROWTH=0 -sSTACK_SIZE=256KB \
    -o "/out/$name.wasm"
}
for v in scalar: avx:"-msimd128 -mavx" avx2:"-msimd128 -mavx2"; do
  n=${v%%:*}; f=${v#*:}
  [ "$n" = scalar ] || build "v02-$n" /v02 rnnoise_data.c $f
  build "main-$n" /main rnnoise_data.c $f
  build "main-little-$n" /main rnnoise_data_little.c $f
  [ "$n" = avx ] && build "mumble-$n" /mumble rnnoise_data.c $f
done
emcc --version | head -1 >/out/EMCC_VERSION
EOF
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$WORK/v02:/v02:ro" -v "$WORK/main:/main:ro" -v "$WORK/mumble:/mumble:ro" -v "$OUT:/out" -v "$WORK/inner.sh:/inner.sh:ro" \
  "$EMSDK_IMAGE" sh /inner.sh
ls -l "$OUT"
