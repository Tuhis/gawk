#!/usr/bin/env bash
# Signs a release's SHA256SUMS with the release key (R47, docs/48 D1) and
# checks the result against the checked-in public key before anything is
# attached. Writes <dir>/SHA256SUMS.minisig; the manifest writer lists it
# under `assets` with no change, and the desktop apps verify it before they
# install anything.
#
#   sign-sums.sh <asset dir> <X.Y.Z>
#   sign-sums.sh --self-test     # the same path end to end with a throwaway
#                                # key; what CI runs on a pull request
#
# The trusted comment is exactly "gawk-broadcast-desktop <X.Y.Z>". It is
# covered by the signature, and the apps read the version they install from
# it rather than from the unsigned manifest (docs/48 D3).
#
# Environment:
#   MINISIGN_SECRET_KEY    the whole encrypted secret key file (a repository
#                          secret, used only by attach-release)
#   MINISIGN_PASSWORD      its passphrase (likewise)
#   MINISIGN_PUBLIC_KEY    optional: the public key file to verify with;
#                          defaults to tools/releases/keys/gawk-release.pub
#   MINISIGN               optional: a minisign binary to use instead of the
#                          pinned download
#
# Fails loudly on a missing secret: a release is never attached unsigned.
# It runs on every push attach-release sees, like the checksum step before
# it; on a push that is not a release, the attach gate discards the file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
self_test=false
if [ "${1:-}" = "--self-test" ]; then
  self_test=true
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077

# minisign 0.12, the author's static Linux build, pinned by hash. The
# tarball's own signature was checked against the author's published key
# (RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3) when this hash
# was pinned.
MINISIGN_VERSION=0.12
MINISIGN_SHA256=9a599b48ba6eb7b1e80f12f36b94ceca7c00b7a5173c95c3efc88d9822957e73
fetch_minisign() {
  [ -z "${MINISIGN:-}" ] || return 0
  curl -fsSL -o "$work/minisign.tar.gz" \
    "https://github.com/jedisct1/minisign/releases/download/${MINISIGN_VERSION}/minisign-${MINISIGN_VERSION}-linux.tar.gz"
  echo "${MINISIGN_SHA256}  $work/minisign.tar.gz" | sha256sum -c --quiet -
  local arch
  case "$(uname -m)" in
    x86_64) arch=x86_64 ;;
    aarch64 | arm64) arch=aarch64 ;;
    *) echo "::error::no minisign build for $(uname -m)" >&2; exit 1 ;;
  esac
  tar -xzf "$work/minisign.tar.gz" -C "$work" "minisign-linux/$arch/minisign" 2>/dev/null
  MINISIGN="$work/minisign-linux/$arch/minisign"
}

if $self_test; then
  fetch_minisign
  dir="$work/assets"
  version=0.0.1
  mkdir -p "$dir"
  echo "self-test" > "$dir/asset.bin"
  (cd "$dir" && sha256sum ./* > "$work/SHA256SUMS" && mv "$work/SHA256SUMS" .)
  MINISIGN_PASSWORD=self-test
  printf '%s\n%s\n' "$MINISIGN_PASSWORD" "$MINISIGN_PASSWORD" |
    "$MINISIGN" -G -p "$work/test.pub" -s "$work/test.key" >/dev/null 2>&1
  MINISIGN_SECRET_KEY=$(cat "$work/test.key")
  MINISIGN_PUBLIC_KEY="$work/test.pub"
else
  dir=${1:?asset directory}
  version=${2:?release version X.Y.Z}
fi
pub=${MINISIGN_PUBLIC_KEY:-$here/keys/gawk-release.pub}

[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "::error::not a release version: $version" >&2; exit 1; }
[ -f "$dir/SHA256SUMS" ] || { echo "::error::$dir/SHA256SUMS not found" >&2; exit 1; }
if [ -z "${MINISIGN_SECRET_KEY:-}" ] || [ -z "${MINISIGN_PASSWORD:-}" ]; then
  echo "::error::MINISIGN_SECRET_KEY and MINISIGN_PASSWORD must both be set: a release is never attached unsigned (docs/48 SU1)" >&2
  exit 1
fi
fetch_minisign

printf '%s\n' "$MINISIGN_SECRET_KEY" > "$work/release.key"
comment="gawk-broadcast-desktop $version"
printf '%s\n' "$MINISIGN_PASSWORD" |
  "$MINISIGN" -S -s "$work/release.key" -m "$dir/SHA256SUMS" \
    -x "$dir/SHA256SUMS.minisig" -t "$comment" >/dev/null 2>&1
rm -f "$work/release.key"

# The key the apps compile in must accept it, with exactly this comment:
# a secret that drifted from the checked-in public key fails here, not in
# every broadcaster's install.
out=$("$MINISIGN" -V -m "$dir/SHA256SUMS" -x "$dir/SHA256SUMS.minisig" -p "$pub")
echo "$out"
got=$(sed -n 's/^Trusted comment: //p' <<<"$out")
[ "$got" = "$comment" ] || { echo "::error::signed comment is '$got', want '$comment'" >&2; exit 1; }
echo "signed $dir/SHA256SUMS as $comment"
