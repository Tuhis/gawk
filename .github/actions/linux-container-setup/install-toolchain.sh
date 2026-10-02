#!/usr/bin/env bash
# Image build only (the Dockerfile beside this file, both stages): rustup under
# /opt and the toolchain rust-toolchain.toml pins, plus llvm-tools for
# cargo-llvm-cov. Jobs never run this; action.yml's own `rustup toolchain
# install` is what catches a PR that bumps the pin.
#
#   install-toolchain.sh <dir holding rust-toolchain.toml>
set -euo pipefail

# Rust under /opt, not $HOME: a container job's HOME is the runner's
# /github/home, a mount over whatever the image had there. CARGO_HOME is only
# set for the install, which is what puts the proxies in /opt/cargo/bin; the
# jobs keep the default registry location (action.yml says why).
export RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo
curl -fsSL https://sh.rustup.rs \
  | sh -s -- -y --profile minimal --default-toolchain none --no-modify-path
export PATH=/opt/cargo/bin:$PATH
cd "$1"
rustup toolchain install
rustup component add llvm-tools-preview
rustup default "$(rustup show active-toolchain | cut -d' ' -f1)"
rustc --version
rm -rf /opt/rustup/downloads /opt/rustup/tmp
