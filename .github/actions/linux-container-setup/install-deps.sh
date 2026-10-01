#!/usr/bin/env bash
# The apt half of linux-container-setup, as a script so the prebuilt CI image
# (Dockerfile beside it) and the action install exactly one list. The image
# records this file's sha256; the action skips the script when the image it
# runs in is current, and runs it — on top of whatever the image has — when it
# is not (a PR that changes this file, or the stock ubuntu:24.04).
#
# Runs as root inside ubuntu:24.04. What each package is for: action.yml.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
  -exec sed -i 's|http://\([a-z0-9.-]*\)\.ubuntu\.com|https://\1.ubuntu.com|g' {} +
cat > /etc/apt/apt.conf.d/99-gawk-ci <<'EOF'
Acquire::ForceIPv4 "true";
Acquire::Retries "1";
Acquire::http::Timeout "15";
Acquire::https::Timeout "15";
EOF
retry() {
  for attempt in 1 2 3; do
    [ "$attempt" = 1 ] || sleep $(((attempt - 1) * 15))
    "$@" && return 0
    echo "::warning::attempt ${attempt}/3 failed: $*"
  done
  return 1
}
# The stock image has no CA bundle, so the first update runs over the
# plain-HTTP index it ships with; everything after it is HTTPS.
if [ ! -e /etc/ssl/certs/ca-certificates.crt ]; then
  find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
    -exec sed -i 's|https://\([a-z0-9.-]*\)\.ubuntu\.com|http://\1.ubuntu.com|g' {} +
  retry apt-get -o APT::Update::Error-Mode=any update
  retry apt-get install -y --no-install-recommends ca-certificates
  find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
    -exec sed -i 's|http://\([a-z0-9.-]*\)\.ubuntu\.com|https://\1.ubuntu.com|g' {} +
fi
retry apt-get -o APT::Update::Error-Mode=any update ||
  echo "::warning::apt-get update never completed cleanly — installing against whatever indexes we have"
retry apt-get install -y --no-install-recommends \
  ca-certificates curl git xz-utils build-essential clang lld cmake pkg-config \
  libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev \
  libpipewire-0.3-dev libspa-0.2-dev libfontconfig-dev libxkbcommon-dev \
  pipewire pipewire-bin wireplumber dbus \
  gstreamer1.0-tools gstreamer1.0-pipewire \
  gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
  gstreamer1.0-plugins-bad gstreamer1.0-plugins-ugly \
  desktop-file-utils binutils file python3 || {
    echo "::error::Build dependencies could not be installed — the Ubuntu archives look unreachable from this runner, not a change in this PR"
    exit 1
  }
