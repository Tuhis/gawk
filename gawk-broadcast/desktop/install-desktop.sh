#!/bin/sh
# Installs (or removes) the launcher entry and icon for gawk-broadcast-gui
# into your own XDG data directory (R44, docs/53 D4). No root, no packages:
# it copies the share/ tree next to this script into
# ${XDG_DATA_HOME:-~/.local/share} and points the entry at the
# gawk-broadcast-gui beside it, so the launcher, taskbar and notifications
# show the gawk icon. It is what INSTALL.md's two manual cp lines do, plus the
# Exec= rewrite those lines leave to you.
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

mkdir -p "$apps"
# Exec= must be absolute: nothing puts the unpacked directory on PATH. The
# path is quoted for the desktop entry's own escaping rules (a space in the
# path is the common case for a Downloads folder).
quoted=$(printf '%s' "$gui" | sed 's/[\\"`$]/\\&/g')
sed "s|^Exec=.*|Exec=\"$quoted\"|" "$entry" > "$apps/$APP_ID.desktop"
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
echo "Exec: $gui"
