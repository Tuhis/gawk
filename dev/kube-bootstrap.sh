#!/bin/sh
# Seeds the dev control plane (docs/41 §4.8): waits for the apiserver, then
# creates the namespace and installs the Ban and Room CRDs — the objects the
# relay and gawk-admin find pre-existing in every real cluster (the relay CHART
# installs them there; here there is no chart, so this stands in).
#
# Plain alpine + busybox wget, NO kubectl: the Kubernetes API accepts
# application/yaml directly, and the kubectl-carrying images cost more than a
# gigabyte for what is a handful of idempotent POSTs — real money on the CI
# runners' RAM-backed docker store. --no-check-certificate matches the
# kubeconfigs: the apiserver self-signs at startup, and this loopback dev
# plane is not a TLS trust exercise.
#
# Each CRD is the chart's OWN template with its few directive lines stripped —
# the same trick gawk-admin's envtest tier uses — so a schema edit in the
# chart is a schema edit here, never a drifting copy.
set -eu

NS="${GAWK_KUBE_NAMESPACE:-gawk}"
API=https://kube-apiserver:6443
TOKEN=$(cut -d, -f1 /kube/token.csv)

req() { # req <method-args...> <path>  — authenticated, body to stdout
  path=$1
  shift
  wget -q --no-check-certificate -O - --header "Authorization: Bearer $TOKEN" "$@" "$API$path"
}
exists() { req "$1" >/dev/null 2>&1; }

echo "kube-bootstrap: waiting for the apiserver"
i=0
until exists /readyz; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "kube-bootstrap: the apiserver never answered /readyz" >&2
    exit 1
  fi
  sleep 2
done

if exists "/api/v1/namespaces/$NS"; then
  echo "kube-bootstrap: namespace $NS exists"
else
  printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"%s"}}' "$NS" > /tmp/ns.json
  req /api/v1/namespaces --header 'Content-Type: application/json' --post-file=/tmp/ns.json >/dev/null
  echo "kube-bootstrap: namespace $NS created"
fi

CRD=/apis/apiextensions.k8s.io/v1/customresourcedefinitions

# BOTH CRDs the relay chart installs, because both are objects gawk-admin
# expects to find pre-existing. The Room one is not optional here just because
# this stack's relay reads its static rooms from a file: with the CRD absent,
# a gawk-admin started with -rooms answers 500 "internal" on every room route,
# which reads as a bug in the portal rather than as a missing object.
install_crd() { # install_crd <crd-name> <file> <label>
  if exists "$CRD/$1"; then
    echo "kube-bootstrap: the $3 CRD exists"
  else
    sed '/{{/d' "$2" > /tmp/crd.yaml
    req "$CRD" --header 'Content-Type: application/yaml' --post-file=/tmp/crd.yaml >/dev/null
    echo "kube-bootstrap: the $3 CRD installed"
  fi
}

await_established() { # await_established <crd-name> <label>
  i=0
  until req "$CRD/$1" | tr -d ' \n' | grep -q '{"type":"Established","status":"True"'; do
    i=$((i + 1))
    if [ "$i" -gt 30 ]; then
      echo "kube-bootstrap: the $2 CRD never became Established" >&2
      exit 1
    fi
    sleep 2
  done
}

install_crd bans.gawk.ioio.fi /bootstrap/crd-ban.yaml Ban
install_crd rooms.gawk.ioio.fi /bootstrap/crd-room.yaml Room

await_established bans.gawk.ioio.fi Ban
await_established rooms.gawk.ioio.fi Room
echo "kube-bootstrap: namespace $NS and the Ban and Room CRDs are ready"
