#!/usr/bin/env bash
# The dev stack's FAST PATH, shared by ci.yml's `dev-stack` and
# `dev-stack-screening` jobs: compile the stack's Go services on the runner,
# where the shared Go cache is (.github/actions/go-shared-cache), and write a
# compose override that packages them with e2e/Dockerfile.prebuilt instead of
# building them from their real Dockerfiles inside BuildKit. `dev-stack`'s
# comment on the step that runs this says what the fast path does not cover.
#
#     .github/scripts/dev-stack-prebuilt.sh OUT_DIR [--screening]
#
# Run from the repository root. Prints the override's path on stdout; the
# caller puts it LAST in COMPOSE_FILE, after any lane override whose services
# it replaces. --screening adds the magenta fixture publisher of R40's
# screening lane (dev/compose.screening.yml), which only that lane runs.
set -euo pipefail

out=${1:?usage: dev-stack-prebuilt.sh OUT_DIR [--screening]}
screening=false
case "${2:-}" in
  "") ;;
  --screening) screening=true ;;
  *) echo "dev-stack-prebuilt.sh: unknown argument: $2" >&2; exit 2 ;;
esac

mkdir -p "$out"
out=$(cd "$out" && pwd)
export CGO_ENABLED=0

# The two modules build side by side: each `go build` already spreads its own
# packages over the CPUs, but the links are serial, and they are most of a
# warm-cache build.
(
  cd gawk-server
  go build -trimpath -ldflags "-s -w -X main.version=dev" -o "$out/gawk-server" ./cmd/gawk-server
  go build -trimpath -ldflags "-s -w" -o "$out/gawk-echo" ./cmd/gawk-echo
  if [ "$screening" = true ]; then
    go build -trimpath -o "$out/gawk-pubsim" ./cmd/gawk-pubsim
  fi
) >&2 &
server_pid=$!
(
  cd gawk-admin
  go build -trimpath -ldflags "-s -w -X main.version=dev" -o "$out/gawk-admin" ./cmd/gawk-admin
  go build -trimpath -o "$out/gawk-fakeidp" ./cmd/gawk-fakeidp
) >&2 &
admin_pid=$!
rc=0
wait "$server_pid" || rc=1
wait "$admin_pid" || rc=1
[ "$rc" = 0 ] || { echo "dev-stack-prebuilt.sh: a build failed" >&2; exit 1; }

cp e2e/Dockerfile.prebuilt "$out/Dockerfile"
{
  echo "services:"
  echo "  relay:"
  echo "    build: { context: \"$out\", dockerfile: Dockerfile, target: relay }"
  echo "  fakeidp:"
  echo "    build: { context: \"$out\", dockerfile: Dockerfile, target: fakeidp }"
  echo "  admin-migrate:"
  echo "    build: { context: \"$out\", dockerfile: Dockerfile, target: admin }"
  echo "  admin:"
  echo "    build: { context: \"$out\", dockerfile: Dockerfile, target: admin }"
  if [ "$screening" = true ]; then
    echo "  pubsim-magenta:"
    echo "    build: { context: \"$out\", dockerfile: Dockerfile, target: pubsim }"
  fi
} > "$out/compose.prebuilt.yml"
ls -l "$out" >&2
echo "$out/compose.prebuilt.yml"
