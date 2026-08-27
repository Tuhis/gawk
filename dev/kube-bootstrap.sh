#!/bin/sh
# Seeds the dev control plane (docs/41 §4.8): waits for the apiserver, then
# creates the namespace and installs the Ban CRD — the two objects the relay
# and gawk-admin find pre-existing in every real cluster (the relay CHART
# installs the CRD there; here there is no chart, so this stands in).
#
# Plain alpine + busybox wget, NO kubectl: the Kubernetes API accepts
# application/yaml directly, and the kubectl-carrying images cost more than a
# gigabyte for what is two idempotent POSTs — real money on the CI runners'
# RAM-backed docker store. --no-check-certificate matches the kubeconfigs:
# the apiserver self-signs at startup, and this loopback dev plane is not a
# TLS trust exercise.
#
# The CRD is the chart's OWN template with its few directive lines stripped —
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
if exists "$CRD/bans.gawk.ioio.fi"; then
  echo "kube-bootstrap: the Ban CRD exists"
else
  sed '/{{/d' /bootstrap/crd-ban.yaml > /tmp/crd.yaml
  req "$CRD" --header 'Content-Type: application/yaml' --post-file=/tmp/crd.yaml >/dev/null
  echo "kube-bootstrap: the Ban CRD installed"
fi

i=0
until req "$CRD/bans.gawk.ioio.fi" | tr -d ' \n' | grep -q '{"type":"Established","status":"True"'; do
  i=$((i + 1))
  if [ "$i" -gt 30 ]; then
    echo "kube-bootstrap: the Ban CRD never became Established" >&2
    exit 1
  fi
  sleep 2
done
echo "kube-bootstrap: namespace $NS and the Ban CRD are ready"
