#!/bin/sh
# Snapshots what the harness's prompt is built from: DESCRIBE of each view and the field catalogue.
set -eu
cd "$(dirname "$0")"
for v in rollups sessions relay; do ./q.sh "DESCRIBE $v" | jq -c '.rows[] | [.[0], .[1]]' > "$v.desc"; done
curl -s -m20 -u "$(cat "$GAWK_TELEMETRY_AUTH_FILE")" "$GAWK_TELEMETRY_URL/v1/fields" > fields.json
