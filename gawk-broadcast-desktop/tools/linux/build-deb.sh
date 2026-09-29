#!/bin/sh
# Builds gawk-broadcast-linux-x86_64.deb (R61, docs/63) from the release
# binary: the tarball's contents laid out under /usr, so dpkg installs the
# launcher entry and icons system-wide and the desktop finds them with no
# install-desktop.sh step. The icon on Wayland IS that desktop entry
# (docs/53 D3), so this is the whole point of the package.
#
#   build-deb.sh <binary> <assets/icon dir> <output .deb> [doc file...]
#
# The doc files (LICENSE, THIRD-PARTY-NOTICES-linux.md, BUILD-INFO.txt) land
# in /usr/share/doc/gawk-broadcast/. Plain dpkg-deb, no cargo-deb or debhelper
# (docs/63 D3): the layout is ten files, and dpkg-dev is already in the build
# container via build-essential. Depends is dpkg-shlibdeps' reading of the
# binary's NEEDED entries against the build container's packages (D4), plus
# what the app loads at runtime, which no linker sees: the windowing
# libraries winit dlopen()s, and the GStreamer plugins and the portal.
#
# No maintainer scripts (D6): the hicolor icon cache and the desktop database
# are refreshed by dpkg file triggers that hicolor-icon-theme and
# desktop-file-utils own.
set -eu

PACKAGE=gawk-broadcast
APP_ID=fi.ioio.gawk.broadcast
BIN=gawk-broadcast-linux
# The address the project's commits already carry; the field must be
# "Name <email>" (Debian Policy 5.6.2).
MAINTAINER='Juho Kuusisto <20241932+Tuhis@users.noreply.github.com>'
# Loaded at runtime (GStreamer elements, the ScreenCast portal) rather than
# linked, so dpkg-shlibdeps cannot find them. INSTALL.md's apt line, minus the
# desktop-specific portal backend, which xdg-desktop-portal's own Recommends
# pulls in for the running desktop.
RUNTIME_DEPENDS='gstreamer1.0-pipewire, gstreamer1.0-plugins-base, gstreamer1.0-plugins-good, gstreamer1.0-plugins-bad, xdg-desktop-portal'
# dlopen()ed by Slint's winit backend and its GL renderer (xkbcommon-dl,
# wayland-sys, x11-dl, glutin), so they are not NEEDED entries either. Every
# desktop has them; a fresh container does not, and test-deb.sh checks each
# versioned soname the binary names resolves after install.
DLOPEN_DEPENDS='libxkbcommon0, libxkbcommon-x11-0, libwayland-client0, libwayland-egl1, libegl1, libgl1, libx11-6, libx11-xcb1, libxcb1, libxcursor1, libxi6, libxrender1'

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <binary> <assets/icon dir> <output .deb> [doc file...]" >&2
  exit 2
fi
bin=$1
icon_dir=$2
out=$3
shift 3
here=$(cd "$(dirname "$0")" && pwd)

[ -x "$bin" ] || { echo "error: $bin is not an executable" >&2; exit 1; }
version=$(tr -d '[:space:]' < "$here/../../version.txt")
case $version in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "error: version.txt holds '$version', not X.Y.Z" >&2; exit 1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root="$work/root"
doc="$root/usr/share/doc/$PACKAGE"
mkdir -p "$root/usr/bin" "$doc" "$root/DEBIAN"

install -m 755 "$bin" "$root/usr/bin/$BIN"
# The share/ tree has one author (assemble-share.sh). It also drops
# install-desktop.sh beside it, which a package must not ship: the entry is
# installed already, and the script would write a second, per-user one.
"$here/assemble-share.sh" "$icon_dir" "$root/usr" >/dev/null
rm "$root/usr/install-desktop.sh"
# Exec= is the bare binary name in the checked-in entry (install-desktop.sh
# rewrites it to an absolute path for the tarball); /usr/bin is on PATH.
grep -qx "Exec=$BIN" "$root/usr/share/applications/$APP_ID.desktop" || {
  echo "error: the desktop entry's Exec= is not '$BIN'" >&2; exit 1; }

for f in "$@"; do
  install -m 644 "$f" "$doc/$(basename "$f")"
done
cat > "$doc/copyright" <<EOF
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: gawk
Source: https://github.com/Tuhis/gawk

Files: *
Copyright: 2026 Juho Kuusisto and the gawk contributors
License: Apache-2.0
 On Debian systems the full text is in /usr/share/common-licenses/Apache-2.0.
 The third-party components built into the binary, and their licences, are
 listed in THIRD-PARTY-NOTICES-linux.md beside this file.
EOF
# A binary package without a changelog is a lintian error; the real one is
# the component's CHANGELOG.md, which this points at.
# The release page's URL, on its own line to stay under 80 columns.
changes="https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v$version"
date=$(date -R -u ${SOURCE_DATE_EPOCH:+-d "@$SOURCE_DATE_EPOCH"})
printf '%s (%s) stable; urgency=medium\n\n  * Release notes:\n    %s\n\n -- %s  %s\n' \
  "$PACKAGE" "$version" "$changes" "$MAINTAINER" "$date" | gzip -9n > "$doc/changelog.gz"
chmod 644 "$doc/changelog.gz" "$doc/copyright"

# dpkg-shlibdeps wants a debian/control naming the package it resolves for.
mkdir -p "$work/src/debian"
printf 'Source: %s\n\nPackage: %s\nArchitecture: amd64\n' "$PACKAGE" "$PACKAGE" > "$work/src/debian/control"
shlibs=$(cd "$work/src" && dpkg-shlibdeps -O "$root/usr/bin/$BIN" 2>/dev/null | sed -n 's/^shlibs:Depends=//p')
[ -n "$shlibs" ] || { echo "error: dpkg-shlibdeps produced no dependencies" >&2; exit 1; }

size=$(du -sk --exclude=DEBIAN "$root" | cut -f1)
cat > "$root/DEBIAN/control" <<EOF
Package: $PACKAGE
Version: $version
Architecture: amd64
Maintainer: $MAINTAINER
Installed-Size: $size
Depends: $shlibs, $DLOPEN_DEPENDS, $RUNTIME_DEPENDS
Section: video
Priority: optional
Homepage: https://gawk.ioio.fi
Description: low-latency screen broadcaster for gawk
 Shares your screen, one window, or one application's audio to viewers
 who watch in the browser at sub-500 ms latency, encoded on your GPU with
 GStreamer. Join by a six-character code; no accounts.
 .
 Needs a hardware H.264 encoder (VA-API, NVENC or Vulkan Video) and a
 Wayland or X11 desktop with the XDG ScreenCast portal.
EOF

find "$root" -type d -exec chmod 755 {} +
mkdir -p "$(dirname "$out")"
dpkg-deb --root-owner-group -Zxz --build "$root" "$out" >/dev/null
echo "$out: $PACKAGE $version"
echo "Depends: $shlibs, $DLOPEN_DEPENDS, $RUNTIME_DEPENDS"
