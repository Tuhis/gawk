#!/usr/bin/env bash
# Per-pod origin/edge assertions for the R20 tier-2 cluster E2E (docs/25
# Decision 7): with a publisher and ~13 subscriber sessions spread across two
# relay pods by kube-proxy's UDP conntrack, exactly one pod must hold the
# broadcast as origin (publisher active, downstream edges attached) and the
# other must serve it as an edge (real local subscribers, fed by an internal
# pull). It also asserts the R18 (docs/23) live viewer count in cluster mode:
# the origin's global count must equal the real viewers summed across both
# pods (edge-pod viewers counted, edge sessions not). Read from each pod's TCP
# ops endpoint via kubectl port-forward — that side-channel is TCP and
# forwards fine.
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
#
# Since R30's striped step the run carries TWO broadcasts (the loadgen-spread
# small one and the striped large one), and their publishers hash to whichever
# pod conntrack picks — so "exactly one origin pod" holds PER BROADCAST KEY,
# never per fleet (the shared statsKey makes keys comparable across pods).
# The edge/viewer/DVR assertions target the loadgen broadcast: the one whose
# origin has an edge with real subscribers attached.
end=$((SECONDS + DEADLINE))
while true; do
  edge_pods=()
  origins_ok=1
  origin_keys=0
  SMALL_KEY=""
  ORIGIN=""
  declare -A ORIGIN_OF=()
  for p in "${PODS[@]}"; do
    st=$(statusz "$p" 2>/dev/null) || continue
    while IFS= read -r k; do
      [ -n "$k" ] || continue
      if [ -n "${ORIGIN_OF[$k]-}" ]; then
        origins_ok=0 # the same broadcast claims origin on two pods
      else
        ORIGIN_OF[$k]=$p
        origin_keys=$((origin_keys + 1))
      fi
    done < <(jq -r '.broadcasts | to_entries[] | select(.value.role == "origin" and .value.publisherActive) | .key' <<<"$st")
    if jq -e '[.broadcasts[] | select(.role == "edge" and .subscribers >= 1)] | length >= 1' \
      >/dev/null <<<"$st"; then
      edge_pods+=("$p")
    fi
    # The loadgen broadcast: served as an edge with real subscribers here.
    k=$(jq -r '.broadcasts | to_entries[] | select(.value.role == "edge" and .value.subscribers >= 1) | .key' <<<"$st" | head -1)
    [ -n "$k" ] && SMALL_KEY=$k
  done
  if [ -n "$SMALL_KEY" ]; then
    ORIGIN=${ORIGIN_OF[$SMALL_KEY]-}
  fi
  if [ "$origins_ok" -eq 1 ] && [ "$origin_keys" -ge 1 ] && [ "${#edge_pods[@]}" -ge 1 ] && [ -n "$ORIGIN" ]; then
    break
  fi
  if [ "$SECONDS" -ge "$end" ]; then
    echo "FAIL: origins_ok=$origins_ok origin_keys=$origin_keys edge pods = ${edge_pods[*]-none} small-key=${SMALL_KEY:-none} after ${DEADLINE}s" >&2
    for p in "${PODS[@]}"; do
      echo "--- $p /statusz:" >&2
      statusz "$p" >&2 || echo "(unreachable)" >&2
    done
    exit 1
  fi
  ORIGIN_OF=()
  sleep 3
done

# R21 (docs/26 Decision 13): the DVR ring lives on the pod serving the
# subscriber, so an EDGE-served DVR viewer builds its ring from datagrams the
# edge pulled from the origin — a different path from an origin-served ring,
# and the only one tier 2 reaches. Poll for it: which pod a session lands on is
# the same probabilistic conntrack spread as the split above.
#
# Asserted on the edge specifically rather than "some pod": an origin-served
# ring is already covered by the tier-1 deep-buffer pass, so accepting either
# here would let the edge path rot unnoticed.
end=$((SECONDS + DEADLINE))
while true; do
  edge_dvr=0
  for p in "${PODS[@]}"; do
    st=$(statusz "$p" 2>/dev/null) || continue
    if jq -e '[.broadcasts[] | select(.role == "edge" and .dvrSubscribers >= 1 and .dvrRingBytes > 0)] | length >= 1' \
      >/dev/null <<<"$st"; then
      edge_dvr=1
      echo "edge DVR ok on $p: $(jq -c '[.broadcasts[] | select(.role == "edge") | {dvrSubscribers, dvrRingBytes, dvrRingGops, dvrResyncs}]' <<<"$st")"
      break
    fi
  done
  [ "$edge_dvr" -eq 1 ] && break
  if [ "$SECONDS" -ge "$end" ]; then
    echo "FAIL: no edge pod is serving a DVR subscriber from a ring after ${DEADLINE}s" >&2
    for p in "${PODS[@]}"; do
      echo "--- $p /statusz:" >&2
      statusz "$p" >&2 || echo "(unreachable)" >&2
    done
    exit 1
  fi
  sleep 3
done

# The loadgen broadcast's origin must also see the edge attached (the
# internal pull is what feeds the edge pod's subscribers).
if ! statusz "$ORIGIN" | jq -e --arg k "$SMALL_KEY" '.broadcasts[$k] | .role == "origin" and .edgeSessions >= 1' >/dev/null; then
  echo "FAIL: origin pod $ORIGIN reports no attached edge sessions for $SMALL_KEY" >&2
  statusz "$ORIGIN" >&2
  exit 1
fi

# R18 (docs/23) live viewer count in cluster mode: the origin's global count
# (G = own local viewers + Σ edge downstream reports) must equal the total
# real viewers summed across both pods — proving the edge pod's viewers ARE
# counted while the edge session itself is NOT. ViewersGlobal is 0 on edges
# (they receive G from upstream) and eventually-consistent on the origin
# (1 Hz pump + edge keepalive reports), so poll to convergence.
# Scoped to the loadgen broadcast: the striped large broadcast has its own
# origin whose viewer population is transient (the striped browser session),
# and mixing the two would make this check depend on pass timing.
vsum() { statusz "$1" | jq --arg k "$SMALL_KEY" '.broadcasts[$k].subscribers // 0'; }
gend=$((SECONDS + 30))
while true; do
  total=0
  for p in "${PODS[@]}"; do
    n=$(vsum "$p" 2>/dev/null) || n=0
    total=$((total + n))
  done
  origin_local=$(statusz "$ORIGIN" | jq --arg k "$SMALL_KEY" '.broadcasts[$k].subscribers // 0') || origin_local=-1
  viewers_global=$(statusz "$ORIGIN" | jq --arg k "$SMALL_KEY" '.broadcasts[$k].viewersGlobal // 0') || viewers_global=-1
  # vg == total proves the count aggregates across pods; total > origin_local
  # proves edge-pod viewers (not just origin-local ones) are in the sum.
  if [ "$viewers_global" -eq "$total" ] && [ "$total" -gt "$origin_local" ]; then
    break
  fi
  if [ "$SECONDS" -ge "$gend" ]; then
    echo "FAIL: R18 viewer count — origin viewersGlobal=$viewers_global, Σ real subscribers across pods=$total, origin-local=$origin_local (want viewersGlobal==total>origin-local)" >&2
    exit 1
  fi
  sleep 2
done
echo "PASS: R18 viewers — origin viewersGlobal=$viewers_global == Σ real subscribers across pods ($total), > origin-local $origin_local (edge viewers counted, edge sessions excluded)"

# R29 (docs/34 §5): parity must survive the origin/edge cascade.
#
# The cascade is exactly where a per-subscriber design can go wrong: parity is
# generated once by the producer at the origin, and an EDGE has to forward the
# right prefix to its own subscribers without ever computing anything. The
# origin cannot know an edge's subscribers' k, which is why the filtering
# happens at the serving pod — the same rule R19 follows for reliable
# conversion (docs/24).
#
# So the claim here is narrow and structural: every pod carrying viewers
# forwarded parity, and no pod ever reported computing any. A pod with
# parityDatagramsForwarded == 0 while it has real subscribers means the
# cascade dropped the symbols somewhere upstream — which is invisible in a
# single-pod test by construction.
parity_pods=0
parity_total=0
for p in "${PODS[@]}"; do
  st=$(statusz "$p") || continue
  # Count the subscribers parity is actually FOR: a reliable/deep-buffer
  # viewer is served none by design (QUIC already recovers its loss), so a pod
  # holding only those must not be accused of losing symbols.
  subs=$(jq '[.broadcasts[].subscriberDetails[]? | select((.internal // false) | not) | select((.parityK // 0) > 0)] | length' <<<"$st")
  fwd=$(jq '[.broadcasts[].parityDatagramsForwarded] | add // 0' <<<"$st")
  parity_total=$((parity_total + fwd))
  if [ "$subs" -gt 0 ]; then
    if [ "$fwd" -le 0 ]; then
      echo "FAIL: R29 parity — pod $p serves $subs parity-negotiated viewer(s) but forwarded no parity; the cascade lost the symbols" >&2
      exit 1
    fi
    parity_pods=$((parity_pods + 1))
  fi
done
if [ "$parity_pods" -lt 2 ]; then
  echo "FAIL: R29 parity — only $parity_pods pod(s) forwarded parity; the cascade was not exercised" >&2
  exit 1
fi
echo "PASS: R29 parity — $parity_total symbols forwarded across $parity_pods pods (origin + edge), none computed by any relay"

# R30 (docs/35 §5.7): striping must survive the cluster. The workflow ran a
# striped browser viewer against the LARGE-frame broadcast through the same
# NodePort spread, so its primary and legs landed on whichever pods conntrack
# chose — cross-pod legs need no coordination because each leg's (member, N)
# filter is static, and this is the first place that claim meets real
# kube-proxy hashing. The session is closed by the time this script runs, so
# the assertion reads the DURABLE counters: an engagement flips
# stripeTransitions (folded into the hub when the subscriber closes) and a
# suppressed primary withholds deltas into stripeSuppressedDatagrams. Both at
# zero across every pod means the striped pass never actually engaged
# in-cluster — a vacuous browser pass this check exists to catch.
stripe_transitions=0
stripe_suppressed=0
for p in "${PODS[@]}"; do
  st=$(statusz "$p") || continue
  # TOTALS, not per-broadcast rows: the striped viewer's serving hub may be
  # a viewerless EDGE that lingered out minutes before this script runs, and
  # the expiry fold (docs/35 §12 finding 3) is what carries its counters
  # into the pod totals — the per-broadcast row is simply gone.
  tr=$(jq '.totals.stripeTransitions // 0' <<<"$st")
  sup=$(jq '.totals.stripeSuppressedDatagrams // 0' <<<"$st")
  stripe_transitions=$((stripe_transitions + tr))
  stripe_suppressed=$((stripe_suppressed + sup))
  echo "  $p stripe: transitions=$tr suppressedDatagrams=$sup legsNow=$(jq '[.broadcasts[].stripeLegs // 0] | add // 0' <<<"$st")"
done
if [ "$stripe_transitions" -lt 1 ] || [ "$stripe_suppressed" -lt 1 ]; then
  echo "FAIL: R30 striping — transitions=$stripe_transitions suppressedDatagrams=$stripe_suppressed across pods; the striped browser pass never engaged in-cluster" >&2
  exit 1
fi
echo "PASS: R30 striping — $stripe_transitions engagement transition(s), $stripe_suppressed primary-suppressed datagrams across the fleet"

echo "PASS: origin=$ORIGIN edges=${edge_pods[*]}"
for p in "${PODS[@]}"; do
  st=$(statusz "$p")
  echo "  $p: $(jq -c '[.broadcasts[] | {role, publisherActive, subscribers, edgeSessions, framesRelayed}]' <<<"$st")"
done
