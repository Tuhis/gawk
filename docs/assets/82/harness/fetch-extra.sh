#!/usr/bin/env bash
# R81 VX0: the voice-like and stationary hums docs/81's corpus lacks
# (docs/82 §12). Run after docs/assets/81/harness/fetch-corpus.sh into the
# same directory:
#   bash docs/assets/82/harness/fetch-extra.sh CORPUS_DIR
# Downloads four Wikimedia Commons files (pinned by sha256) and decodes each
# to 48 kHz mono f32le, looped to 60 s. Not committed, like NS0's corpus.
#   hum-woman   Woman humming a nondescript tune.wav (CC0), a person humming
#   hum-chant   20260602 145937 Dingjue Om mani padme hum.ogg (CC0), a
#               murmured chant: voiced, low, no consonants to speak of
#   hum-mains   Mains hum 60 Hz.ogg (CC BY-SA 3.0)
#   hum-aircon  Air conditioner hum (Gravity Sound).wav (CC BY 4.0)
set -euo pipefail
OUT=$(realpath -m "${1:?usage: fetch-extra.sh CORPUS_DIR}")
mkdir -p "$OUT/dl"
C=https://upload.wikimedia.org/wikipedia/commons
while read -r name sha url; do
  f="$OUT/dl/$name"
  [ -f "$f" ] || curl -sSfL -A "gawk-vx0/1 (docs/82 spike)" -o "$f" "$url"
  echo "$sha  $f" | sha256sum -c --quiet -
  ffmpeg -nostdin -loglevel error -y -stream_loop -1 -i "$f" -ac 1 -ar 48000 -t 60 -f f32le "$OUT/${name%.*}.f32"
done <<EOF
hum-woman.wav c616a7ae88e50215101d8f657318682b11ab9888943fc29f4c89903a8215f740 $C/5/5b/Woman_humming_a_nondescript_tune.wav
hum-chant.ogg a34a2badc8185e9b9725f509dc532424a9db96246deea8d61de9afab5bc25d03 $C/a/a0/20260602_145937_Dingjue_Om_mani_padme_hum.ogg
hum-mains.ogg dd11780ff2161f3a2f0ced1ed7128f9ddf6772e2df2b1bf246222abc2552aee5 $C/a/ab/Mains_hum_60_Hz.ogg
hum-aircon.wav bac546ddd06be356facb3dd9c842635afad312412fe058e14d7e2fb07e4f34d0 $C/9/99/Air_conditioner_hum_%28Gravity_Sound%29.wav
EOF
ls -l "$OUT"/hum-*.f32
