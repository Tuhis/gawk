#!/usr/bin/env bash
# Frame-size traces from a real encoder (docs/73 §4), rate-controlled like the
# broadcaster: VBR target 75 % of the ceiling, GOP = fps/2, no B-frames, zero
# latency. Needs ffmpeg with libx264; the 60 s Mandelbrot zoom renders at
# ~0.16x real time, so expect 10-15 minutes.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${UPLINK_OUT:-$ROOT/tools/uplinklab/out}
mkdir -p "$OUT/traces"
cd "$OUT/traces"
gen() { # name size target peak vbv
  ffmpeg -hide_banner -loglevel error -y -f lavfi -i "mandelbrot=s=$2:rate=60" -t 60 \
    -c:v libx264 -preset veryfast -tune zerolatency -g 30 -keyint_min 30 -sc_threshold 0 -bf 0 \
    -b:v "$3" -maxrate "$4" -bufsize "$5" -f h264 "$1.264"
  ffprobe -v error -show_entries packet=size,flags -of csv=p=0 "$1.264" > "$1.csv"
  rm "$1.264"
}
gen r1080p60 1920x1080 9M 12M 6M &
gen r1440p60 2560x1440 37500k 50M 25M &
wait
echo "traces in $OUT/traces"
