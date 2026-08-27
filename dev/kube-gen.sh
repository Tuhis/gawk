#!/bin/sh
# Generates the dev control plane's credentials, once (docs/41 §4.8).
#
# The kube-apiserver refuses to start without a service-account signing
# keypair, and every client needs a credential — this writes both into the
# `kube-pki` volume, plus two kubeconfigs for the two worlds that need one:
#
#   /kube/kubeconfig          server https://kube-apiserver:6443 — for the
#                             relay and gawk-admin containers (KUBECONFIG)
#   /out/kubeconfig           server https://127.0.0.1:${KUBE_API_PORT} — for
#                             the DEVELOPER: the break-glass surface docs/42
#                             §9.6 describes, against the dev stack:
#                               kubectl --kubeconfig dev/generated/kubeconfig get bans
#
# IDEMPOTENT: everything persists in the volume, so the token and keys are
# stable across `docker compose up` runs — regenerating them would invalidate
# the kubeconfig a developer's shell still points at.
#
# THROWAWAY CREDENTIALS BY DESIGN: the token authorizes AlwaysAllow on an
# apiserver published on loopback only, holding nothing but dev Ban CRs and a
# Lease. TLS verification is skipped for the same reason — the apiserver
# self-signs at startup, and pinning a dev CA would buy nothing here.
set -eu

KUBE=/kube
OUT=/out
API_PORT="${KUBE_API_PORT:-6445}"

if [ -f "$KUBE/kubeconfig" ] && [ -f "$KUBE/sa.key" ] && [ -f "$KUBE/token.csv" ]; then
  echo "kube-gen: credentials exist; leaving them alone"
else
  echo "kube-gen: generating the dev control plane's credentials"
  openssl genrsa -out "$KUBE/sa.key" 2048 2>/dev/null
  openssl rsa -in "$KUBE/sa.key" -pubout -out "$KUBE/sa.pub" 2>/dev/null

  TOKEN=$(openssl rand -hex 24)
  # token,user,uid,"groups" — the shape --token-auth-file wants.
  printf '%s,gawk-dev,gawk-dev,system:masters\n' "$TOKEN" > "$KUBE/token.csv"

  cat > "$KUBE/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: gawk-dev
    cluster:
      server: https://kube-apiserver:6443
      insecure-skip-tls-verify: true
contexts:
  - name: gawk-dev
    context: {cluster: gawk-dev, user: gawk-dev}
current-context: gawk-dev
users:
  - name: gawk-dev
    user: {token: $TOKEN}
EOF

  mkdir -p "$KUBE/self-signed"
  # World-readable on purpose: the relay container may run as the developer's
  # own uid (HOST_UID), and this volume guards nothing beyond the dev stack.
  chmod 755 "$KUBE" "$KUBE/self-signed"
  chmod 644 "$KUBE/kubeconfig" "$KUBE/sa.pub" "$KUBE/token.csv" "$KUBE/sa.key"
fi

# The host copy is derived on every run: the published port may have changed.
sed "s#https://kube-apiserver:6443#https://127.0.0.1:$API_PORT#" "$KUBE/kubeconfig" > "$OUT/kubeconfig"
chmod 644 "$OUT/kubeconfig"
echo "kube-gen: host kubeconfig at dev/generated/kubeconfig (server https://127.0.0.1:$API_PORT)"
