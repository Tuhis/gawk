#!/bin/sh
# Tidy every Go module that builds gawk-server through a local replace.
#
# gawk-admin, gawk-telemetry, gawk-sampler and gawk-mumble-bridge reach
# gawk-server via `replace github.com/Tuhis/gawk/gawk-server => ../gawk-server`,
# so a bump that moves gawk-server's go.mod (x/net dragging x/sys along, say)
# changes their module graph too. Renovate only runs its gomodTidy in the
# go.mod files it edited, so those modules were left untidy: CI's tidy job and
# every build in them failed with "updates to go.mod needed", and so did
# gen-notices.py's `go list` in the same postUpgradeTasks run (PRs #608, #611,
# #614). Renovate runs this before gen-notices.py; it is equally the command to
# run by hand after editing gawk-server/go.mod.
#
# The module list is derived from the replace lines, so a new dependent module
# is covered without editing this file.
set -eu
cd "$(dirname "$0")/../.."

(cd gawk-server && go mod tidy)
for gomod in */go.mod; do
	if grep -q '^replace github.com/Tuhis/gawk/gawk-server => \.\./gawk-server' "$gomod"; then
		dir=$(dirname "$gomod")
		echo "go mod tidy in $dir"
		(cd "$dir" && go mod tidy)
	fi
done
