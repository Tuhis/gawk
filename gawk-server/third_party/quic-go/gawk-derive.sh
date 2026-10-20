#!/usr/bin/env bash
# Re-derives this directory from upstream: quic-go at the version
# gawk-server's go.mod requires, without its tests and its example, interop
# and test harnesses, plus gawk.patch. Review the result with `git diff`; on
# an unchanged version it is empty. See GAWK-PATCH.md.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
mod=github.com/quic-go/quic-go
version=$(go -C "$HERE/../.." list -m -f '{{.Version}}' "$mod")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# Outside any module, so gawk-server's replace does not redirect the download.
src=$(cd "$tmp" && GOFLAGS= go mod download -json "$mod@$version" |
  sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$src" ] || { echo "gawk-derive: could not download $mod@$version" >&2; exit 1; }

find "$HERE" -mindepth 1 -maxdepth 1 \
  ! -name GAWK-PATCH.md ! -name gawk.patch ! -name gawk-derive.sh -exec rm -rf {} +
(cd "$src" && find . -type f \
  \( \( -name '*.go' ! -name '*_test.go' \) -o -name LICENSE -o -name go.mod -o -name go.sum \) \
  ! -path './example/*' ! -path './integrationtests/*' ! -path './interop/*' \
  ! -path './internal/mocks/*' ! -path '*/testdata/*' \
  -print0 | tar --null -T - -cf -) | tar -xf - -C "$HERE"
chmod -R u+w "$HERE"
patch -d "$HERE" -p1 --no-backup-if-mismatch < "$HERE/gawk.patch"
echo "gawk-derive: $mod $version + gawk.patch in $HERE"
