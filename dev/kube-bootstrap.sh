#!/bin/sh
# Seeds the dev control plane (docs/41 §4.8): waits for the apiserver, then
# creates the namespace and installs the Ban CRD — the two objects the relay
# and gawk-admin find pre-existing in every real cluster (the relay CHART
# installs the CRD there; here there is no chart, so this stands in).
#
# The CRD is the chart's OWN template with its few directive lines stripped —
# the same trick gawk-admin's envtest tier uses — so a schema edit in the
# chart is a schema edit here, never a drifting copy.
set -eu

export KUBECONFIG=/kube/kubeconfig
NS="${GAWK_KUBE_NAMESPACE:-gawk}"
CRD_TEMPLATE=/bootstrap/crd-ban.yaml

echo "kube-bootstrap: waiting for the apiserver"
i=0
until kubectl get --raw /readyz >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "kube-bootstrap: the apiserver never answered /readyz" >&2
    kubectl get --raw /readyz || true
    exit 1
  fi
  sleep 2
done

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
sed '/{{/d' "$CRD_TEMPLATE" | kubectl apply -f -
kubectl wait --for=condition=Established crd/bans.gawk.ioio.fi --timeout=60s
echo "kube-bootstrap: namespace $NS and the Ban CRD are ready"
