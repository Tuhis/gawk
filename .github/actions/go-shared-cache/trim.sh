#!/usr/bin/env bash
# One trim pass over this node's shared GOCACHE (action.yml has the why).
# Usage: trim.sh <cache root>/go <hours>. Run under flock by action.yml.
set -euo pipefail
go=$1
hours=$2
start=$(date +%s)

# Exactly the files Go's own trim removes: action (-a) and output (-d)
# entries in the two-hex-digit subdirectories, by mtime, which Go refreshes
# on use.
freed=$(find "$go/build" -mindepth 2 -maxdepth 2 -type f -name '*-[ad]' \
  -mmin +$((hours * 60)) -printf '%s\n' -delete |
  awk '{ n++; b += $1 } END { printf "%d files, %.1f GiB", n, b / 2^30 }')
sizes=$(du -sh "$go/build" "$go/mod" 2>/dev/null | awk '{ sub(".*/", "", $2); printf "%s%s=%s", sep, $2, $1; sep = " " }')

log=$go/trim.log
echo "$(date -u +%FT%TZ) ${CI_CACHE_NODE:-$(hostname)}: removed $freed unused for ${hours}h in $(($(date +%s) - start))s; now $sizes" >>"$log"
tail -n 200 "$log" >"$log.tmp" && mv "$log.tmp" "$log"
touch "$go/trim.stamp"
