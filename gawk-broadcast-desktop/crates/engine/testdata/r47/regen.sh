#!/bin/sh
# Test vectors for gawk-engine's R47 verifier (docs/48 SU2), made with the
# real minisign so the Rust side is checked against the tool CI signs with.
# Keys are throwaway test keys, never the release key. From this directory:
#
#   docker run --rm -v "$PWD:/out" -v "$PWD/regen.sh:/f.sh:ro" ubuntu:24.04 sh /f.sh
#
# Regenerating changes every signature and the test key; the tests read the
# files, so nothing else needs editing.
set -eu
apt-get update -qq >/dev/null && apt-get install -y -qq minisign >/dev/null 2>&1
out=/out
mkdir -p "$out"
cd "$(mktemp -d)"
minisign -G -W -p a.pub -s a.key >/dev/null
minisign -G -W -p b.pub -s b.key >/dev/null
printf 'gawk test asset\n' > "$out/asset.bin"
asset_sum=$(sha256sum "$out/asset.bin" | cut -d' ' -f1)
printf 'build info\n' > info.txt
info_sum=$(sha256sum info.txt | cut -d' ' -f1)
# The shape `sha256sum ./*` writes in the attach job.
printf '%s  ./BUILD-INFO.txt\n%s  ./gawk-broadcast-windows-x86_64.exe\n' "$info_sum" "$asset_sum" > "$out/SHA256SUMS"
cp "$out/SHA256SUMS" s
minisign -S -s a.key -m s -x "$out/sig-2.1.0.minisig" -t 'gawk-broadcast-desktop 2.1.0' >/dev/null
minisign -S -s a.key -m s -x "$out/sig-1.9.0.minisig" -t 'gawk-broadcast-desktop 1.9.0' >/dev/null
minisign -S -s a.key -m s -x "$out/sig-foreign.minisig" -t 'something-else 2.1.0' >/dev/null
minisign -S -s b.key -m s -x "$out/sig-other-key.minisig" -t 'gawk-broadcast-desktop 2.1.0' >/dev/null
sed -n 2p a.pub > "$out/test-key.pub"
chown -R 1000:1000 "$out"
ls -l "$out"
