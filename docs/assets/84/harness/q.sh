#!/bin/sh
# Runs one statement against a gawk-telemetry read listener's /v1/query.
#   GAWK_TELEMETRY_URL       e.g. http://localhost:8081 (a port-forward to the read Service)
#   GAWK_TELEMETRY_AUTH_FILE a file holding "user:password" for basic auth
set -eu
curl -s -m90 -u "$(cat "$GAWK_TELEMETRY_AUTH_FILE")" -X POST "$GAWK_TELEMETRY_URL/v1/query" \
  -d "$(jq -n --arg s "$1" '{sql:$s}')"
