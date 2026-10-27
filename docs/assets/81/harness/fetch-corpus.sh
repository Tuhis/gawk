#!/usr/bin/env bash
# R80 NS0: the speech and noise the offline run mixes (docs/81 §12).
#   bash docs/assets/81/harness/fetch-corpus.sh OUT_DIR
# Downloads six Wikimedia Commons files (pinned by sha256) and decodes the
# first 60 s of each to 48 kHz mono f32le with ffmpeg. The audio is not
# committed: three are CC BY-SA and none of it belongs in the repository.
#   speech-a  En-Javan-article.oga (CC BY-SA 4.0), a Spoken Wikipedia reading
#   speech-b  En-Antidisestablishmentarianism-article.oga (CC BY-SA 3.0)
#   speech-c  MacBook Neo Spoken Article by EclipseGMD (EN).ogg (CC BY-SA 4.0)
#   noise-keyboard    Laptop keyboard.ogg (public domain)
#   noise-restaurant  Restaurant ambience.ogg (public domain)
#   noise-street      Sunday in the city street noise1.ogg (public domain)
set -euo pipefail
OUT=$(realpath -m "${1:?usage: fetch-corpus.sh OUT_DIR}")
mkdir -p "$OUT/dl"
C=https://upload.wikimedia.org/wikipedia/commons
while read -r name sha url; do
  f="$OUT/dl/$name"
  [ -f "$f" ] || curl -sSfL -A "gawk-ns0/1 (docs/81 spike)" -o "$f" "$url"
  echo "$sha  $f" | sha256sum -c --quiet -
  ffmpeg -nostdin -loglevel error -y -i "$f" -ac 1 -ar 48000 -t 60 -f f32le "$OUT/${name%.*}.f32"
done <<EOF
speech-a.oga 585106f81259621c7b761ee0dfc6e07ec010ddf32fb8f9b4ad3d9cf60fd63b6f $C/6/63/En-Javan-article.oga
speech-b.oga 572137fad89d031556043d7e42d4f161de4f7e2967ce7266a2d6bbd981540bb6 $C/4/46/En-Antidisestablishmentarianism-article.oga
speech-c.ogg 4bab20ae42885f770ae03ce12b2fcff743b1ceec033d2caa0419c74c44625612 $C/6/6d/MacBook_Neo_Spoken_Article_by_EclipseGMD_%28EN%29.ogg
noise-keyboard.ogg 687c8961b838f75344d99a9762934e1741a390d942a2b4b375d01d0d99cfdd20 $C/6/67/Laptop_keyboard.ogg
noise-restaurant.ogg 0aeb1fa7a3d9647d3284e97d9880b6375b54db2eda6a517c64e3bb02ac8a51cc $C/b/b5/Restaurant_ambience.ogg
noise-street.ogg 50e5eb59b296580340898f16e737f826a9fd7fef6fc45b07bb3e0318a9e58495 $C/5/5d/Sunday_in_the_city_street_noise1.ogg
EOF
ls -l "$OUT"
