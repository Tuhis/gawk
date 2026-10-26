#!/usr/bin/env bash
# Regenerates testdata/tone-440hz-10ms.opus, the e2e voice fixture that
# gawk-mumblesim -play sends: 2.5 s of a 440 Hz tone, mono 48 kHz, Opus in
# 10 ms frames at 40 kbit/s (docs/80 OD31), as length-prefixed packets.
# Needs an ffmpeg with libopus (`ffmpeg -encoders | grep libopus`).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
ffmpeg -hide_banner -loglevel error -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=2.5" \
  -ac 1 -c:a libopus -application lowdelay -frame_duration 10 -b:a 40k "$tmp/tone.ogg"
(cd "$here/.." && go run ./cmd/gawk-mumblesim fixture < "$tmp/tone.ogg" > "$here/tone-440hz-10ms.opus")
echo "wrote $here/tone-440hz-10ms.opus ($(wc -c < "$here/tone-440hz-10ms.opus") bytes)"
