#!/usr/bin/env bash
# relay-up.sh "<netem args>": (re)starts the lab relay with a publisher on its
# loopback, so the uplink is never shaped, and netem on the relay's egress,
# so every viewer's download is. Prints the broadcast ID.
#
# The publisher is the field case's stream (docs/91 §2): the large fixture
# padded to 294 KB keyframes and 26 KB deltas at 30 fps with a 15-frame GOP,
# that is 10.7 Mbit/s, 44 % of it keyframes, a keyframe every 0.5 s.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${DVRLAB_OUT:-$ROOT/tools/dvrlab/out}
netem=$1
docker rm -f dvrlab-relay >/dev/null 2>&1 || true
docker run -d --name dvrlab-relay --network dvrlab --cap-add NET_ADMIN \
  -v "$OUT/bin:/b:ro" -v "$OUT/pki:/pki:ro" dvrlab-alpine sleep infinity >/dev/null
docker exec dvrlab-relay sh -c "tc qdisc add dev eth0 root netem $netem"
key=$(printf 'ab%.0s' $(seq 32))
docker exec -d dvrlab-relay sh -c "/b/gawk-server -addr :4433 -metrics-addr 0.0.0.0:9100 \
  -cert-file /pki/relay.pem -key-file /pki/relay.key -stats-key $key \
  -conn-rate-limit 1000 -conn-burst-limit 1000 -log-level ${RELAY_LOG_LEVEL:-info} ${RELAY_ARGS:-} > /tmp/relay.log 2>&1"
docker exec dvrlab-relay sh -c 'for i in $(seq 100); do curl -sf http://127.0.0.1:9100/healthz >/dev/null && exit 0; sleep 0.1; done; exit 1'
docker exec -d dvrlab-relay sh -c "/b/gawk-pubsim -url https://127.0.0.1:4433 -insecure -fixture large -fps 30 \
  -pad-keyframe-bytes 294000 -pad-delta-bytes 26300 > /tmp/pub.log 2>&1"
for _ in $(seq 100); do
  id=$(docker exec dvrlab-relay sh -c "sed -n 's/^GAWK_PUBSIM_ID=//p' /tmp/pub.log" || true)
  [ -n "$id" ] && { sleep 3; echo "$id"; exit 0; }
  sleep 0.1
done
echo "dvrlab: the publisher never got a broadcast ID" >&2
exit 1
