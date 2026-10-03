#!/bin/bash
# The .deb's install test (R61, docs/63 DB2): run as root in a FRESH
# container of a supported release (ubuntu:24.04, debian:trixie), never in the
# build container, whose -dev packages would satisfy any Depends the package
# forgot.
#
#   test-deb.sh <package .deb>
#
# It proves, in order: lintian finds no error; apt resolves every Depends
# from the stock archive; the installed binary resolves every shared library,
# linked or dlopen()ed; the plugin files behind every GStreamer element the
# app creates are present; the launcher entry validates, names an icon that
# is installed and handles gawk:// links; and a
# purge removes every file the package installed.
set -euo pipefail

deb=$(realpath "${1:?package .deb}")
APP_ID=fi.ioio.gawk.broadcast
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
# xdg-utils for G9's handler query (R66 docs/68 D11); not a dependency.
apt-get install -y -qq --no-install-recommends lintian desktop-file-utils xdg-utils >/dev/null

echo "=== lintian"
# Errors fail; warnings and info are printed for review (no manual page, for
# one, is expected: the app is a GUI with no flags).
lintian --fail-on error --no-tag-display-limit "$deb"

echo "=== apt install"
apt-get install -y -qq --no-install-recommends "$deb"
pkg=$(dpkg-deb -f "$deb" Package)
dpkg -s "$pkg" | sed -n '/^Version/p;/^Depends/p'

echo "=== shared libraries"
missing=$(ldd /usr/bin/gawk-broadcast-linux | grep 'not found' || true)
if [ -n "$missing" ]; then
  echo "::error::installed from the .deb, gawk-broadcast-linux still misses libraries (Depends is incomplete):"
  echo "$missing"
  exit 1
fi

echo "=== dlopen()ed libraries"
# winit and glutin load the windowing libraries by soname at runtime, so ldd
# cannot see them. Every versioned soname the binary carries as a string must
# resolve once the package is installed; one added by a dependency bump fails
# here instead of on a user's desktop.
ldconfig
wanted=$(grep -aoE 'lib[A-Za-z0-9_+-]+\.so\.[0-9]+' /usr/bin/gawk-broadcast-linux | sort -u)
cached=$(ldconfig -p | awk 'NR > 1 {print $1}' | sort -u)
unresolved=$(comm -23 <(echo "$wanted") <(echo "$cached"))
if [ -n "$unresolved" ]; then
  echo "::error::sonames the binary loads at runtime are not installed by the Depends:"
  echo "$unresolved"
  exit 1
fi
# ...and each must come from a package the .deb names itself. Resolving is
# not enough: the GStreamer plugin packages pull most of the windowing stack
# in transitively, which hid a missing libxrender1 until this check.
direct=$(dpkg-deb -f "$deb" Depends | tr ',|' '\n\n' | sed 's/(.*//; s/[[:space:]]//g' | sort -u)
indirect=$(while read -r so; do
  path=$(ldconfig -p | awk -v s="$so" '$1 == s {print $NF; exit}')
  owner=$(dpkg -S "$(realpath "$path")" | head -1 | cut -d: -f1)
  grep -qxF "$owner" <<<"$direct" || echo "$so (from $owner)"
done <<<"$wanted")
if [ -n "$indirect" ]; then
  echo "::error::sonames the binary loads come from packages the .deb does not depend on directly:"
  echo "$indirect"
  exit 1
fi
echo "$(wc -l <<<"$wanted") sonames resolve, each from a direct dependency"

echo "=== runtime GStreamer elements"
# The elements the app builds pipelines from, which no linker sees — the
# reason RUNTIME_DEPENDS exists in build-deb.sh. gst-inspect is not a
# dependency, so ask the registry through the plugin files instead.
# pipewiresrc; videoconvertscale/videorate/appsink/audioconvert; pulsesrc;
# h264parse, vah264enc/vapostproc, nvh264enc/cudaupload.
plugins=$(dpkg -L gstreamer1.0-pipewire gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad)
for lib in libgstpipewire.so libgstvideoconvertscale.so libgstvideorate.so libgstapp.so \
           libgstaudioconvert.so libgstpulseaudio.so libgstvideoparsersbad.so libgstva.so \
           libgstnvcodec.so; do
  grep -q "/$lib\$" <<<"$plugins" || { echo "::error::$lib is not installed by the Depends"; exit 1; }
done

echo "=== launcher entry and icon"
entry=/usr/share/applications/$APP_ID.desktop
desktop-file-validate "$entry"
grep -qx 'Exec=gawk-broadcast-linux %u' "$entry"
grep -qx 'MimeType=x-scheme-handler/gawk;' "$entry"
# G9: the package registers gawk:// through desktop-file-utils' trigger
# alone (it sets no default), and xdg-mime resolves it to our entry.
handler=$(xdg-mime query default x-scheme-handler/gawk)
[ "$handler" = "$APP_ID.desktop" ] || {
  echo "::error::xdg-mime names '$handler' as the gawk:// handler, not $APP_ID.desktop"; exit 1; }
grep -qx "Icon=$APP_ID" "$entry"
test -f /usr/share/icons/hicolor/scalable/apps/$APP_ID.svg
test -f /usr/share/icons/hicolor/256x256/apps/$APP_ID.png
test -x "$(command -v gawk-broadcast-linux)"
test ! -e /usr/install-desktop.sh

echo "=== purge"
files=$(dpkg -L "$pkg" | while read -r f; do [ -f "$f" ] && echo "$f"; done)
apt-get purge -y -qq "$pkg" >/dev/null
left=$(while read -r f; do [ -e "$f" ] && echo "$f"; done <<<"$files" || true)
if [ -n "$left" ]; then
  echo "::error::purge left files behind:"
  echo "$left"
  exit 1
fi
echo "ok: $pkg installs, resolves its libraries and plugins, ships its launcher entry and icon, and purges cleanly"
