#!/usr/bin/env bash
# Starts real murmur with gawk-mmproxy beside it, laid out as a Kubernetes
# pod (docs/92 PX0's topology): a pause container owns the network
# namespace, and murmur, the `routes` init step and `serve` join it. Prints
# the environment that enables the integration tests.
#
#   eval "$(gawk-mmproxy/testdata/topology.sh up [1.5|1.6])"
#   (cd gawk-mmproxy && go test ./internal/itest/)
#   gawk-mmproxy/testdata/topology.sh down
#
# Knobs (env):
#   GAWK_MMPROXY_PREFIX     container name prefix (default gawk-test-mmproxy)
#   GAWK_MMPROXY_IMAGE      the gawk-mmproxy image; default: built from the
#                           repository as gawk-mmproxy:test
#   GAWK_MMPROXY_MURMUR_HOST  murmur's host= (default blank: every address)
#   GAWK_MMPROXY_MURMUR_ENV   extra `-e K=V` words for murmur (the bridge's
#                           murmur.sh passes its operator prerequisites)
#
# The tests dial the pod's container address, NOT a port published on
# 127.0.0.1: docker's userland proxy (docker-proxy) sits behind every
# published localhost port and turns murmur's reset into a FIN, which is
# exactly the signal under test (docs/92 D6). Kubernetes has no such hop:
# kube-proxy only rewrites addresses. A client dialling murmur directly is a
# "native" client; murmur sees it at the docker network's gateway (printed
# as GAWK_TEST_MMPROXY_NATIVE_ADDR).
set -euo pipefail

cmd="${1:-up}"
version="${2:-1.6}"
case "$version" in
  1.5) image="mumblevoip/mumble-server:v1.5.939" ;;
  1.6) image="mumblevoip/mumble-server:v1.6.1001" ;;
  *) echo "unknown murmur version $version (want 1.5 or 1.6)" >&2; exit 2 ;;
esac
prefix="${GAWK_MMPROXY_PREFIX:-gawk-test-mmproxy}"
supw="${GAWK_MMPROXY_SUPERUSER_PASSWORD:-gawk-test-superuser}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

down() {
  for n in serve murmur pause; do
    docker rm -f "$prefix-$n" >/dev/null 2>&1 || true
  done
}

announces() {
  # Captured, not piped: under pipefail, `docker logs | grep -q` fails once
  # grep exits on its first match and docker logs dies of SIGPIPE.
  local out
  out=$(docker logs "$prefix-murmur" 2>&1 || true)
  grep -c "registering server as public\|Announcing server" <<<"$out" || true
}

wait_murmur() { # [announces seen before this start]
  local before="${1:-0}"
  for _ in $(seq 1 200); do
    if (( $(announces) > before )); then
      return 0
    fi
    sleep 0.1
  done
  echo "murmur did not come up:" >&2
  docker logs "$prefix-murmur" >&2 || true
  return 1
}

start_murmur() {
  local extra=()
  if [[ -n "${GAWK_MMPROXY_MURMUR_ENV:-}" ]]; then
    # shellcheck disable=SC2206
    extra=($GAWK_MMPROXY_MURMUR_ENV)
  fi
  local host=()
  if [[ -n "${GAWK_MMPROXY_MURMUR_HOST:-}" ]]; then
    host=(-e "MUMBLE_CONFIG_HOST=$GAWK_MMPROXY_MURMUR_HOST")
  fi
  docker run -d --name "$prefix-murmur" --network "container:$prefix-pause" \
    -e MUMBLE_SUPERUSER_PASSWORD="$supw" -e MUMBLE_CONFIG_WELCOMETEXT="" \
    -e MUMBLE_CONFIG_AUTOBANATTEMPTS="${GAWK_MMPROXY_AUTOBAN_ATTEMPTS:-6}" \
    -e MUMBLE_CONFIG_AUTOBANTIMEFRAME=60 -e MUMBLE_CONFIG_AUTOBANTIME=30 \
    "${host[@]}" "${extra[@]}" "$image" >/dev/null
  wait_murmur
}

case "$cmd" in
  up)
    down
    img="${GAWK_MMPROXY_IMAGE:-}"
    if [[ -z "$img" ]]; then
      img=gawk-mmproxy:test
      docker build -q -f "$repo/gawk-mmproxy/deploy/Dockerfile" -t "$img" "$repo" >/dev/null
    fi
    # The pod sandbox: owns the namespace and its published ports, with
    # IPv6 on (so lo has ::1) as a Kubernetes pod has by default.
    docker run -d --name "$prefix-pause" \
      --sysctl net.ipv6.conf.all.disable_ipv6=0 --sysctl net.ipv6.conf.lo.disable_ipv6=0 \
      alpine:3.24 sleep infinity >/dev/null
    start_murmur
    # Murmur's SuperUser password, set again on the running server (the
    # bridge's murmur.sh explains why).
    for _ in $(seq 1 20); do
      if [[ "$version" == 1.5 ]]; then
        docker exec -u 10000 "$prefix-murmur" /usr/bin/mumble-server -ini /data/mumble_server_config.ini -supw "$supw" >/dev/null 2>&1 && break
      else
        docker exec -u 10000 "$prefix-murmur" /usr/bin/mumble-server --ini /data/mumble_server_config.ini --set-su-pw "$supw" >/dev/null 2>&1 && break
      fi
      sleep 0.5
    done
    # The init container: root, NET_ADMIN, and NET_RAW for the file
    # capability's sake (docs/92 F14). Always exits 0.
    docker run --rm --network "container:$prefix-pause" --user 0 \
      --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW "$img" routes >&2
    # The sidecar: nonroot, NET_RAW only (docs/92 D5).
    docker run -d --name "$prefix-serve" --network "container:$prefix-pause" \
      --cap-drop ALL --cap-add NET_RAW --read-only "$img" serve >/dev/null
    ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$prefix-pause")
    for _ in $(seq 1 100); do
      curl -fsS "http://$ip:2114/healthz" >/dev/null 2>&1 && break
      sleep 0.1
    done
    gw=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' "$prefix-pause")
    echo "export GAWK_TEST_MMPROXY=1"
    echo "export GAWK_TEST_MMPROXY_MURMUR=$ip:64738"
    echo "export GAWK_TEST_MMPROXY_PROXY=$ip:64739"
    echo "export GAWK_TEST_MMPROXY_OPS=$ip:2114"
    echo "export GAWK_TEST_MMPROXY_NATIVE_ADDR=$gw"
    echo "export GAWK_TEST_MMPROXY_PREFIX=$prefix"
    echo "export GAWK_TEST_MMPROXY_VERSION=$version"
    echo "export GAWK_TEST_MMPROXY_MURMUR_HOST=${GAWK_MMPROXY_MURMUR_HOST:-}"
    echo "export GAWK_TEST_MMPROXY_SUPERUSER_PASSWORD=$supw"
    ;;
  restart-murmur)
    # Restart murmur alone: the namespace, and so the routes, survive (F7).
    n=$(announces)
    docker restart "$prefix-murmur" >/dev/null
    wait_murmur "$n"
    ;;
  stop-murmur) docker stop "$prefix-murmur" >/dev/null ;;
  start-murmur) n=$(announces); docker start "$prefix-murmur" >/dev/null; wait_murmur "$n" ;;
  down) down ;;
  *) echo "usage: $0 up [1.5|1.6] | restart-murmur | stop-murmur | start-murmur | down" >&2; exit 2 ;;
esac
