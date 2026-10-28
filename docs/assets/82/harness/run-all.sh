#!/usr/bin/env bash
# R81 VX0: the whole measurement as recorded in docs/82 §12.
# Run from the repo root:  bash docs/assets/82/harness/run-all.sh WORK_DIR
#
# Needs: node, ffmpeg, curl, and `(cd e2e && npm ci)` with Playwright's
# Chromium installed (on NixOS: PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
# and LD_LIBRARY_PATH=$NIX_LD_LIBRARY_PATH). Chromium's fake microphone
# needs no sound server. WORK_DIR receives the corpus and the captures; only
# docs/assets/82/results/ is kept in the repository.
set -euo pipefail
REPO=$(pwd)
WORK=$(realpath -m "${1:?usage: run-all.sh WORK_DIR}")
H=$REPO/docs/assets/82/harness
mkdir -p "$WORK"
[ -f "$WORK/corpus/speech-a.f32" ] || bash "$REPO/docs/assets/81/harness/fetch-corpus.sh" "$WORK/corpus"
[ -f "$WORK/corpus/hum-woman.f32" ] || bash "$H/fetch-extra.sh" "$WORK/corpus"
node "$H/capture.mjs" --corpus "$WORK/corpus" --out "$WORK/cap" --parallel 4
node "$H/vx0.mjs" --corpus "$WORK/corpus" --captures "$WORK/cap"
node "$H/report.mjs" --gates "$(cat "$H/report-gates.txt")"
