#!/bin/sh
# Installs (or removes) the launcher entry and icon for gawk-broadcast-gui
# into your own XDG data directory (R44, docs/53 D4). No root, no packages:
# it copies the share/ tree next to this script into
# ${XDG_DATA_HOME:-~/.local/share} and points the entry at the
# gawk-broadcast-gui beside it, so the launcher, taskbar and notifications
# show the gawk icon. By hand, for a path without spaces or quotes, it is:
#
#   cp -r share/applications share/icons ~/.local/share/
#   sed -i "s|^Exec=.*|Exec=$PWD/gawk-broadcast-gui|" \
#     ~/.local/share/applications/fi.ioio.gawk.broadcast.desktop
#
# The script handles the desktop entry's escaping rules for any path.
#
#   ./install-desktop.sh              install for this user
#   ./install-desktop.sh --uninstall  remove exactly what install put there
#
# The share/ tree is assembled by CI into the release tarball; from a source
# checkout, build it with `go run ./tools/icon generate` and lay the files
# out as the tarball does (see gawk-broadcast/README.md).
set -eu

APP_ID=fi.ioio.gawk.broadcast
here=$(cd "$(dirname "$0")" && pwd)
share="$here/share"
data="${XDG_DATA_HOME:-$HOME/.local/share}"
apps="$data/applications"
icons="$data/icons/hicolor"

# Cache refreshes are best-effort: the desktop picks the files up without
# them, just later.
refresh() {
  command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$apps" 2>/dev/null || true
  command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -q -t "$icons" 2>/dev/null || true
}

case "${1:-}" in
  --uninstall)
    rm -f "$apps/$APP_ID.desktop"
    for f in "$icons"/*/apps/"$APP_ID".png "$icons"/scalable/apps/"$APP_ID".svg; do
      [ -e "$f" ] && rm -f "$f"
    done
    refresh
    echo "removed the $APP_ID launcher entry and icons from $data"
    exit 0
    ;;
  "")
    ;;
  *)
    echo "usage: $0 [--uninstall]" >&2
    exit 2
    ;;
esac

entry="$share/applications/$APP_ID.desktop"
gui="$here/gawk-broadcast-gui"
[ -f "$entry" ] || { echo "$entry not found — run this from the unpacked release directory" >&2; exit 1; }
[ -x "$gui" ] || { echo "$gui is not an executable next to this script" >&2; exit 1; }
# A `%` in the path cannot be launched through the freedesktop stack at
# all: GLib resolves the binary from the *unexpanded* Exec= argument, so
# `%%` looks for a file literally named that and a single `%` is mangled as
# a field code — either way the entry is silently dropped. Refusing beats
# installing an entry the desktop will never show (PR #328 review).
case "$gui" in
  *%*) echo "cannot install a launcher entry from a directory whose path contains '%' ($here): desktop entries cannot launch it. Move the folder and run this again." >&2; exit 1 ;;
esac

mkdir -p "$apps"
# Exec= must be absolute: nothing puts the unpacked directory on PATH. The
# path is quoted for the desktop entry's own rules, which are two layers
# deep: the value is first unescaped as a key-file string (where `\\` is a
# backslash and a lone `\"` is an invalid escape), and only then are the
# quoting rules applied, under which `"`, `` ` ``, `$` and `\` are
# backslash-escaped. So each of those needs TWO backslashes in the file —
# the spec's own example is four backslashes for one literal backslash and
# `\\$` for a dollar. (`%` is refused above; see there.) A space is merely
# the common case for a Downloads folder. The line is then appended with
# printf, not substituted with sed: a sed replacement re-interprets `&`,
# `\` and the delimiter, which is exactly how the first version of this
# script mangled any path containing one. Key order inside the group is
# irrelevant. Backslashes first (one becomes four), then the other three
# (each gains two); in this order the added backslashes are never
# re-escaped. Verified against desktop-file-validate and a real GLib
# launch in the PR #328 review.
quoted=$(printf '%s' "$gui" | sed -e 's/\\/\\\\\\\\/g' -e 's/["`$]/\\\\&/g')
{ grep -v '^Exec=' "$entry"; printf 'Exec="%s"\n' "$quoted"; } > "$apps/$APP_ID.desktop"
chmod 644 "$apps/$APP_ID.desktop"

n=0
for src in "$share"/icons/hicolor/*/apps/"$APP_ID".*; do
  [ -e "$src" ] || continue
  size=$(basename "$(dirname "$(dirname "$src")")")
  mkdir -p "$icons/$size/apps"
  cp "$src" "$icons/$size/apps/"
  n=$((n + 1))
done
[ "$n" -gt 0 ] || { echo "no icons found under $share/icons/hicolor" >&2; exit 1; }

refresh
echo "installed the $APP_ID launcher entry ($n icon files) into $data"
# printf, not echo: dash's echo interprets backslash escapes in the path.
printf 'Exec: %s\n' "$gui"
