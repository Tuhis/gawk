#!/usr/bin/env bash
# Re-derives this directory from upstream: quinn-proto as published on
# crates.io at the version the workspace's Cargo.lock resolves, checked
# against the crates.io index checksum, without the crate's own Cargo.lock,
# plus gawk.patch. Review the result with `git diff`; on an unchanged
# version it is empty. See GAWK-PATCH.md.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
LOCK="$HERE/../../Cargo.lock"
crate=quinn-proto
version=$(awk -v n="$crate" '$0 == "name = \"" n "\"" {getline; gsub(/version = |"/, ""); print; exit}' "$LOCK")
[ -n "$version" ] || { echo "gawk-derive: $crate not in $LOCK" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://static.crates.io/crates/$crate/$crate-$version.crate" -o "$tmp/crate.tar.gz"
# The patched copy has no registry checksum in Cargo.lock, so take it from the
# index: one JSON line per published version.
want=$(curl -fsSL "https://index.crates.io/qu/in/$crate" | python3 -c '
import json, sys
v = sys.argv[1]
for line in sys.stdin:
    e = json.loads(line)
    if e["vers"] == v:
        print(e["cksum"])
' "$version")
have=$(sha256sum "$tmp/crate.tar.gz" | cut -d' ' -f1)
if [ -z "$want" ] || [ "$want" != "$have" ]; then
  echo "gawk-derive: $crate $version checksum ${have} does not match the index (${want:-missing})" >&2
  exit 1
fi
tar -xzf "$tmp/crate.tar.gz" -C "$tmp"

find "$HERE" -mindepth 1 -maxdepth 1 \
  ! -name GAWK-PATCH.md ! -name gawk.patch ! -name gawk-derive.sh -exec rm -rf {} +
(cd "$tmp/$crate-$version" && tar -cf - --exclude=./Cargo.lock --exclude=./.cargo-ok .) | tar -xf - -C "$HERE"
chmod -R u+w "$HERE"
patch -d "$HERE" -p1 --no-backup-if-mismatch --quiet < "$HERE/gawk.patch"
echo "gawk-derive: $crate $version + gawk.patch in $HERE"
