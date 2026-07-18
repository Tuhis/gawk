#!/usr/bin/env bash
# Per-pod origin/edge assertions for the R20 tier-2 cluster E2E (docs/25
# Decision 7): with a publisher and ~13 subscriber sessions spread across two
# relay pods by kube-proxy's UDP conntrack, exactly one pod must hold the
# broadcast as origin (publisher active, downstream edges attached) and the
# other must serve it as an edge (real local subscribers, fed by an internal
# pull). Read from each pod's TCP ops endpoint via kubectl port-forward —
# that side-channel is TCP and forwards fine.
#
# Usage: cluster-assert.sh <namespace> [deadline-seconds]
set -euo pipefail

NS=${1:?usage: cluster-assert.sh <namespace> [deadline-seconds]}
DEADLINE=${2:-120}

mapfile -t PODS < <(kubectl -n "$NS" get pods -l app.kubernetes.io/name=gawk-server \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if [ "${#PODS[@]}" -ne 2 ]; then
  echo "expected 2 relay pods in $NS, found ${#PODS[@]}: ${PODS[*]-}" >&2
  exit 1
fi

declare -A PORT
i=0
for p in "${PODS[@]}"; do
  PORT[$p]=$((21200 + i))
  kubectl -n "$NS" port-forward "pod/$p" "${PORT[$p]}:2112" >/dev/null 2>&1 &
  i=$((i + 1))
done
trap 'kill $(jobs -p) 2>/dev/null || true' EXIT
sleep 2

statusz() { curl -fsS --max-time 5 "http://127.0.0.1:${PORT[$1]}/statusz"; }

# The conntrack spread is probabilistic per 5-tuple; poll until the split
# shows or the deadline passes. jq -e exits non-zero on false, so each check
# is a plain boolean.
end=$((SECONDS + DEADLINE))
while true; do
  origin_pods=()
  edge_pods=()
  for p in "${PODS[@]}"; do
    st=$(statusz "$p" 2>/dev/null) || continue
    if jq -e '[.broadcasts[] | select(.role == "origin" and .publisherActive)] | length == 1' \
      >/dev/null <<<"$st"; then
      origin_pods+=("$p")
    fi
    if jq -e '[.broadcasts[] | select(.role == "edge" and .subscribers >= 1)] | length >= 1' \
      >/dev/null <<<"$st"; then
      edge_pods+=("$p")
    fi
  done
  if [ "${#origin_pods[@]}" -eq 1 ] && [ "${#edge_pods[@]}" -ge 1 ]; then
    break
  fi
  if [ "$SECONDS" -ge "$end" ]; then
    echo "FAIL: origin pods = ${origin_pods[*]-none}, edge pods = ${edge_pods[*]-none} after ${DEADLINE}s" >&2
    for p in "${PODS[@]}"; do
      echo "--- $p /statusz:" >&2
      statusz "$p" >&2 || echo "(unreachable)" >&2
    done
    exit 1
  fi
  sleep 3
done

# The origin must also see the edge attached (the internal pull is what feeds
# the edge pod's subscribers).
ORIGIN=${origin_pods[0]}
if ! statusz "$ORIGIN" | jq -e '[.broadcasts[] | select(.role == "origin" and .edgeSessions >= 1)] | length == 1' >/dev/null; then
  echo "FAIL: origin pod $ORIGIN reports no attached edge sessions" >&2
  statusz "$ORIGIN" >&2
  exit 1
fi

echo "PASS: origin=$ORIGIN edges=${edge_pods[*]}"
for p in "${PODS[@]}"; do
  st=$(statusz "$p")
  echo "  $p: $(jq -c '[.broadcasts[] | {role, publisherActive, subscribers, edgeSessions, framesRelayed}]' <<<"$st")"
done
