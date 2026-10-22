#!/usr/bin/env bash
# Stages a gawk-app build into this package's dist/, where gawk-relay embeds
# it (docs/76 §6.1 item 6). The release image and the CI binaries run this
# before `go build ./cmd/gawk-relay`; a build without it embeds the
# placeholder page instead (embed.go).
#
#   stage.sh                 build gawk-app (npm ci + tsc + vite) and stage it
#   stage.sh --from <dir>    stage an existing build made with --base=./
#   stage.sh --clean         remove the staged build (back to the placeholder)
#
# The app is built with a relative base, so the same bytes serve under the
# console's /app/ and at the named tier's /. Its static config.js is removed:
# the relay generates config.js itself (relayUrl, relayCertHashes,
# requirePublishSecret: false), and a stale copy in the embed could shadow it.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
dist="$here/dist"

clean() {
  find "$dist" -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
}

case "${1:-}" in
  --clean)
    clean
    exit 0
    ;;
  --from)
    src=${2:?--from needs a directory}
    ;;
  "")
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    (
      cd "$repo/gawk-app"
      [ -d node_modules ] || npm ci --no-audit --no-fund
      npx tsc -b
      npx vite build --base=./ --outDir "$work/app" --emptyOutDir
    )
    src="$work/app"
    ;;
  *)
    echo "usage: $0 [--from <dir> | --clean]" >&2
    exit 2
    ;;
esac

if [ ! -f "$src/index.html" ]; then
  echo "stage.sh: $src has no index.html — not a gawk-app build" >&2
  exit 1
fi
# A root-absolute asset path would break under /app/; refuse it here rather
# than ship a console page that loads nothing.
if grep -qE '(src|href)="/assets/' "$src/index.html"; then
  echo "stage.sh: $src/index.html uses root-absolute assets; build it with --base=./" >&2
  exit 1
fi

mkdir -p "$dist"
clean
cp -R "$src"/. "$dist"/
rm -f "$dist/config.js"
echo "staged $(find "$dist" -type f ! -name .gitkeep | wc -l | tr -d ' ') files into $dist"
