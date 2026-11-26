#!/usr/bin/env bash
# The deploy files, applied for real (docs/92 PX3 criterion 3): a kind
# cluster (containerd, and kindnet, which enforces NetworkPolicy), a stock
# murmur with gawk-mmproxy patched into its pod by kustomization.yaml, and
# probe pods. It checks what docs/92 promises of these exact files:
#
#   1. a probe in a pod labelled as the bridge reaches murmur through the
#      proxy, and murmur logs the header's address (uid 65532 + the file
#      capability under containerd: allowPrivilegeEscalation must be true);
#   2. a pod without the label can't reach the proxy's port at all (D9);
#   3. with the routes init container denied NET_ADMIN, murmur's pod still
#      starts and is Ready, and the proxy refuses with a FIN (D4, D5, D15);
#   4. with the proxy failing closed while murmur stays up (murmur's port
#      pointed elsewhere), murmur's pod stays Ready and in its endpoints
#      (D15);
#   5. killing `serve` makes the pod briefly NotReady, then Ready again
#      after the container restarts (D15, §8).
#
#   gawk-mmproxy/deploy/k8s/kind-test.sh            # builds, creates, tests, deletes
#   KEEP_CLUSTER=1 gawk-mmproxy/deploy/k8s/kind-test.sh
#
# Env: KIND_CLUSTER (default gawk-mmproxy), KIND_NODE_IMAGE, MMPROXY_IMAGE
# (default: built from the repository as gawk-mmproxy:kind), KIND_KUBECONFIG
# (default: a temporary file; the caller's kubeconfig is never written).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
cluster="${KIND_CLUSTER:-gawk-mmproxy}"
image="${MMPROXY_IMAGE:-gawk-mmproxy:kind}"
ns=mmproxy-kind
ctx="kind-$cluster"
k() { kubectl --context "$ctx" -n "$ns" "$@"; }
fail() { echo "::error::kind-test: $*" >&2; k get pods -o wide >&2 || true; k logs deploy/murmur --all-containers --tail=40 >&2 || true; exit 1; }
work=$(mktemp -d)
# Its own kubeconfig: never write the caller's.
export KUBECONFIG="${KIND_KUBECONFIG:-$work/kubeconfig}"

cleanup() {
  rm -rf "$work"
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then kind delete cluster --name "$cluster" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

if [[ -z "${MMPROXY_IMAGE:-}" ]]; then
  docker build -q -f "$repo/gawk-mmproxy/deploy/Dockerfile" -t "$image" "$repo" >/dev/null
fi
# The probe: a static binary in a tiny image, test only.
(cd "$repo/gawk-mmproxy" && CGO_ENABLED=0 go build -o "$work/probe" ./testdata/probe)
printf 'FROM alpine:3.24\nCOPY probe /probe\nENTRYPOINT ["/probe"]\n' > "$work/Dockerfile"
docker build -q -t gawk-mmproxy-probe:kind "$work" >/dev/null

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --wait 120s
fi
kind export kubeconfig --name "$cluster" >/dev/null
load() {
  # `kind load` imports with --all-platforms, which fails for a pulled
  # multi-platform image under Docker's containerd image store ("content
  # digest ... not found"); import the one platform by hand instead.
  kind load docker-image --name "$cluster" "$1" >/dev/null 2>&1 && return 0
  local node
  for node in $(kind get nodes --name "$cluster"); do
    docker save --platform "linux/$(go env GOARCH)" "$1" | docker exec -i "$node" ctr --namespace=k8s.io images import - >/dev/null
  done
}
# murmur is pulled by the node itself, from Docker Hub.
for i in "$image" gawk-mmproxy-probe:kind; do load "$i"; done
kubectl --context "$ctx" create namespace "$ns" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null

render() { # the shipped files, with the image under test
  kubectl kustomize "$here" | sed "s#ghcr.io/tuhis/gawk-mmproxy:[0-9][^ ]*#$image#"
}

probe() { # label src -> one word
  local label="$1" src="$2" name="probe-$RANDOM"
  k run "$name" --image=gawk-mmproxy-probe:kind --image-pull-policy=Never --restart=Never \
    --labels="app.kubernetes.io/name=$label" -- -addr mumble-mmproxy:64739 -src "$src" >/dev/null
  k wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$name" --timeout=60s >/dev/null 2>&1 || true
  k logs "$name" 2>/dev/null | head -1
  k delete pod "$name" --wait=false >/dev/null 2>&1 || true
}

murmur_log() { k logs deploy/murmur -c murmur 2>/dev/null; }
ready_pod() { k get pods -l app.kubernetes.io/name=murmur -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}'; }
in_endpoints() { [[ -n "$(k get endpointslices -l kubernetes.io/service-name=murmur -o jsonpath='{.items[*].endpoints[?(@.conditions.ready==true)].addresses[*]}')" ]]; }

deploy() { # the shipped files, then an optional JSON patch for one variation
  render | k apply -f - >/dev/null
  if [[ -n "${1:-}" ]]; then
    k patch deploy murmur --type=json -p "$1" >/dev/null
  fi
  k rollout status deploy/murmur --timeout=180s >/dev/null || fail "murmur's rollout"
  sleep 3
}

# The index of a container by name, for the JSON patches.
cindex() { # kind name
  k get deploy murmur -o json | python3 -c "import json,sys; d=json.load(sys.stdin); print([c['name'] for c in d['spec']['template']['spec']['$1']].index('$2'))"
}

echo "1. the shipped files: a bridge-labelled probe reaches murmur with its header's address"
deploy
got=$(probe gawk-mumble-bridge 203.0.113.7:40001)
[[ "$got" == ok ]] || fail "bridge probe: $got (want ok)"
murmur_log | grep -q "New connection: 203.0.113.7:" || fail "murmur never logged 203.0.113.7"
echo "   ok"

echo "2. a pod without the bridge's label can't reach the proxy (NetworkPolicy)"
got=$(probe something-else 203.0.113.8:40001)
[[ "$got" == timeout || "$got" == refused ]] || fail "unlabelled probe: $got (want timeout)"
murmur_log | grep -q "New connection: 203.0.113.8:" && fail "murmur saw the unlabelled probe"
echo "   ok ($got)"

echo "3. routes init denied NET_ADMIN: murmur's pod still Ready, the proxy refuses with a FIN"
deploy "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/initContainers/$(cindex initContainers mmproxy-routes)/securityContext/capabilities/add\",\"value\":[\"NET_RAW\"]}]"
[[ "$(ready_pod)" == True ]] || fail "murmur's pod not Ready without the routes"
in_endpoints || fail "murmur's pod not in its endpoints without the routes"
got=$(probe gawk-mumble-bridge 203.0.113.9:40001)
[[ "$got" == eof ]] || fail "probe without routes: $got (want eof)"
echo "   ok"

echo "4. the proxy failing closed with murmur up: the pod stays Ready and in its endpoints"
deploy "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/$(cindex containers mmproxy)/args\",\"value\":[\"serve\",\"-murmur-port\",\"1\"]}]"
got=$(probe gawk-mumble-bridge 203.0.113.10:40001)
[[ "$got" == eof ]] || fail "probe with murmur's port pointed elsewhere: $got (want eof)"
[[ "$(ready_pod)" == True ]] || fail "murmur's pod not Ready while the proxy fails closed"
in_endpoints || fail "murmur's pod left its endpoints while the proxy fails closed"
echo "   ok"

echo "5. killing serve: briefly NotReady, then Ready after the restart"
deploy
node=$(kubectl --context "$ctx" get nodes -o jsonpath='{.items[0].metadata.name}')
cid=$(docker exec "$node" crictl ps --name '^mmproxy$' -q | head -1)
[[ -n "$cid" ]] || fail "no mmproxy container on $node"
restarts0=$(k get pods -l app.kubernetes.io/name=murmur -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="mmproxy")].restartCount}')
docker exec "$node" crictl stop -t 0 "$cid" >/dev/null
for _ in $(seq 60); do
  r=$(k get pods -l app.kubernetes.io/name=murmur -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="mmproxy")].restartCount}')
  [[ "$r" -gt "$restarts0" && "$(ready_pod)" == True ]] && break
  sleep 1
done
[[ "$r" -gt "$restarts0" ]] || fail "mmproxy never restarted"
[[ "$(ready_pod)" == True ]] || fail "murmur's pod not Ready after mmproxy's restart"
got=$(probe gawk-mumble-bridge 203.0.113.11:40001)
[[ "$got" == ok ]] || fail "probe after the restart: $got (want ok)"
echo "   ok"
echo "kind-test: all passed"
