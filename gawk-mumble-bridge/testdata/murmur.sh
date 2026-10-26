#!/usr/bin/env bash
# Starts the murmur instances the gawk-mumble-bridge integration tests run
# against, and prints the environment that enables them.
#
#   eval "$(gawk-mumble-bridge/testdata/murmur.sh up)"          # murmur 1.5
#   eval "$(gawk-mumble-bridge/testdata/murmur.sh up 1.6)"      # murmur 1.6
#   (cd gawk-mumble-bridge && go test -race ./...)
#   gawk-mumble-bridge/testdata/murmur.sh down
#
# Three servers, because three behaviours cannot share one:
#
#   main     the operator prerequisites of docs/80 §5: autoban off, a
#            username rule that allows the "{nick} [web]" form (spaces), a
#            message limit that does not bind on the test's admin client.
#            (The rule is written with doubled backslashes: murmur reads its
#            ini through QSettings, which unescapes them once.)
#   strict   the username rule murmur's sample ini documents as the default,
#            [-=\w\[\]\{\}\(\)\@\|\.]+ (no spaces), set EXPLICITLY, for
#            the InvalidUsername fallback (docs/80 §11.5). Measured
#            2026-10-26: murmur 1.5.939 and 1.6.870 with no `username`
#            setting accept "a b" and "x [web]", so the compiled-in default
#            is more permissive than the ini comment says; 1.6 refuses
#            non-ASCII letters ("jörg"), 1.5 accepts them.
#   autoban  a low autoban threshold (3 connections a minute, banned 10 s),
#            for the 4016 autoban case (D18).
#
# Every server gets the same SuperUser password (the tests create the
# temporary-channel parent and kick or mute as SuperUser). Nothing here is a
# secret: the servers listen on 127.0.0.1 only and live as long as the test.
set -euo pipefail

cmd="${1:-up}"
version="${2:-1.5}"
case "$version" in
  1.5) image="mumblevoip/mumble-server:v1.5.939" ;;
  1.6) image="mumblevoip/mumble-server:v1.6.870" ;;
  *) echo "unknown murmur version $version (want 1.5 or 1.6)" >&2; exit 2 ;;
esac
prefix="${GAWK_MURMUR_PREFIX:-gawk-test-murmur}"
base="${GAWK_MURMUR_BASE_PORT:-64740}"
supw="${GAWK_MURMUR_SUPERUSER_PASSWORD:-gawk-test-superuser}"

down() {
  for n in main strict autoban; do
    docker rm -f "$prefix-$n" >/dev/null 2>&1 || true
  done
}

start() { # name port extra-env...
  local name="$1" port="$2"
  shift 2
  docker run -d --rm --name "$prefix-$name" \
    -p "127.0.0.1:$port:64738/tcp" \
    -e MUMBLE_SUPERUSER_PASSWORD="$supw" \
    -e MUMBLE_CONFIG_WELCOMETEXT="" \
    "$@" "$image" >/dev/null
}

wait_ready() { # name
  # murmur opens its port before it has generated its certificate and opened
  # its database; the last start-up line is the public-registration notice.
  local name="$1"
  for _ in $(seq 1 200); do
    if docker logs "$prefix-$name" 2>&1 | grep -q "registering server as public\|Announcing server"; then
      return 0
    fi
    sleep 0.1
  done
  echo "murmur $prefix-$name did not come up:" >&2
  docker logs "$prefix-$name" >&2 || true
  return 1
}

case "$cmd" in
  up)
    down
    start main "$base" \
      -e MUMBLE_CONFIG_AUTOBANATTEMPTS=0 \
      -e 'MUMBLE_CONFIG_USERNAME=[-=\\w\\[\\]\\{\\}\\(\\)\\@\\|\\. ]+' \
      -e MUMBLE_CONFIG_MESSAGELIMIT=100 -e MUMBLE_CONFIG_MESSAGEBURST=200
    start strict "$((base + 1))" \
      -e MUMBLE_CONFIG_AUTOBANATTEMPTS=0 \
      -e 'MUMBLE_CONFIG_USERNAME=[-=\\w\\[\\]\\{\\}\\(\\)\\@\\|\\.]+' \
      -e MUMBLE_CONFIG_MESSAGELIMIT=100 -e MUMBLE_CONFIG_MESSAGEBURST=200
    start autoban "$((base + 2))" \
      -e MUMBLE_CONFIG_AUTOBANATTEMPTS=3 -e MUMBLE_CONFIG_AUTOBANTIMEFRAME=60 \
      -e MUMBLE_CONFIG_AUTOBANTIME=10
    wait_ready main; wait_ready strict; wait_ready autoban
    # murmur 1.6.870's image sets MUMBLE_SUPERUSER_PASSWORD before the first
    # start, and that password does not survive it (SuperUser is then
    # refused with WrongUserPW; measured 2026-10-26). Setting it again on
    # the running server works on both versions.
    for n in main strict autoban; do
      if [[ "$version" == 1.5 ]]; then
        docker exec -u 10000 "$prefix-$n" /usr/bin/mumble-server -ini /data/mumble_server_config.ini -supw "$supw" >/dev/null 2>&1
      else
        docker exec -u 10000 "$prefix-$n" /usr/bin/mumble-server --ini /data/mumble_server_config.ini --set-su-pw "$supw" >/dev/null 2>&1
      fi
    done
    echo "export GAWK_TEST_MURMUR=127.0.0.1:$base"
    echo "export GAWK_TEST_MURMUR_STRICT=127.0.0.1:$((base + 1))"
    echo "export GAWK_TEST_MURMUR_AUTOBAN=127.0.0.1:$((base + 2))"
    echo "export GAWK_TEST_MURMUR_SUPERUSER_PASSWORD=$supw"
    echo "export GAWK_TEST_MURMUR_VERSION=$version"
    ;;
  down) down ;;
  *) echo "usage: $0 up [1.5|1.6] | down" >&2; exit 2 ;;
esac
