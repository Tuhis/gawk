#!/bin/sh
# Lays out the freedesktop share/ tree the Linux tarball carries (R44,
# docs/53 D4) from the repository's icon derivatives and desktop entry:
#
#   share/applications/fi.ioio.gawk.broadcast.desktop
#   share/icons/hicolor/scalable/apps/fi.ioio.gawk.broadcast.svg
#   share/icons/hicolor/<N>x<N>/apps/fi.ioio.gawk.broadcast.png   (N per assets/icon/png)
#
# One script rather than a cp list in ci.yml so the layout has exactly one
# author, and so a developer can produce the same tree from a checkout.
#
#   assemble-share.sh <assets/icon dir> <output dir>
set -eu
APP_ID=fi.ioio.gawk.broadcast
here=$(cd "$(dirname "$0")" && pwd)
icon_dir=${1:?assets/icon directory}
out=${2:?output directory}
share="$out/share"

[ -f "$icon_dir/gawk.svg" ] || { echo "$icon_dir/gawk.svg not found" >&2; exit 1; }
mkdir -p "$share/applications" "$share/icons/hicolor/scalable/apps"
cp "$here/$APP_ID.desktop" "$share/applications/"
cp "$icon_dir/gawk.svg" "$share/icons/hicolor/scalable/apps/$APP_ID.svg"
n=0
for png in "$icon_dir"/png/gawk-*.png; do
  [ -e "$png" ] || continue
  size=$(basename "$png" .png)
  size=${size#gawk-}
  mkdir -p "$share/icons/hicolor/${size}x${size}/apps"
  cp "$png" "$share/icons/hicolor/${size}x${size}/apps/$APP_ID.png"
  n=$((n + 1))
done
[ "$n" -gt 0 ] || { echo "no PNGs under $icon_dir/png — run 'go run ./tools/icon generate'" >&2; exit 1; }
cp "$here/install-desktop.sh" "$out/"
chmod 755 "$out/install-desktop.sh"
echo "assembled $share ($n PNG sizes + scalable) and $out/install-desktop.sh"
