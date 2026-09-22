#!/bin/sh
# Assembles and signs gawk-broadcast-macos.app (R52, docs/54 D13/D14).
#
#   tools/macos/bundle.sh <release binary> <output directory>
#
# Writes <output directory>/gawk-broadcast-macos.app and nothing else. The
# version comes from gawk-broadcast-desktop/version.txt (release-please
# maintains it), so a bundle can never carry a version the binary does not.
#
# Signing: SIGN_IDENTITY names the codesign identity. Unset means an ad-hoc
# signature ("-"), which is what every pull-request build is (docs/54 D13 —
# PRs never sign): it runs on the machine it is opened on via Privacy &
# Security → Open Anyway, and any TCC grant it collects dies with the build.
# The hardened runtime and the entitlements are applied either way, so an
# ad-hoc build exercises the same runtime restrictions a notarized one does.
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <release binary> <output directory>" >&2
	exit 2
fi
bin=$1
out=$2
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

[ -x "$bin" ] || { echo "error: $bin is not an executable" >&2; exit 1; }
version=$(tr -d '[:space:]' < "$root/gawk-broadcast-desktop/version.txt")
case $version in
	[0-9]*.[0-9]*.[0-9]*) ;;
	*) echo "error: version.txt holds '$version', not X.Y.Z" >&2; exit 1 ;;
esac

app="$out/gawk-broadcast-macos.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin" "$app/Contents/MacOS/gawk-broadcast-macos"
sed "s/@VERSION@/$version/g" "$here/Info.plist.in" > "$app/Contents/Info.plist"
printf 'APPL????' > "$app/Contents/PkgInfo"
plutil -lint "$app/Contents/Info.plist" >/dev/null

# --options runtime is the hardened runtime notarization requires. --timestamp
# only for a real identity: an ad-hoc signature cannot carry a secure
# timestamp, and asking for one makes codesign contact Apple for nothing.
identity=${SIGN_IDENTITY:--}
if [ "$identity" = "-" ]; then
	timestamp=--timestamp=none
else
	timestamp=--timestamp
fi
codesign --force --sign "$identity" --options runtime "$timestamp" \
	--entitlements "$here/entitlements.plist" "$app"
codesign --verify --deep --strict "$app"
echo "$app: $version, signed ${identity}"
