# ghcr.io/tuhis/gawk-desktop-ci: the container broadcast-desktop.yml's `test`
# and `linux` jobs run in (docs/58 D12). It is ubuntu:24.04 with this
# action's install-deps.sh and the toolchain rust-toolchain.toml pins already
# applied — the 61-110 s each of those jobs spent on apt and rustup, every
# run, on runners whose image store dies with the pod.
#
# Still ubuntu:24.04, so the Linux binary's glibc floor is what it was (OD14),
# and still separate from the runner image (ghcr.io/tuhis/actions-runner,
# built in the ioio repo), which D12 keeps out of the Linux build on purpose.
#
# Built from the repository root by .github/workflows/desktop-ci-image.yml.
# action.yml says why a stale copy is harmless.
FROM ubuntu:24.04

LABEL org.opencontainers.image.source="https://github.com/Tuhis/gawk"
LABEL org.opencontainers.image.description="gawk-broadcast-desktop Linux CI: ubuntu:24.04 + GStreamer/PipeWire build and test deps + the pinned Rust toolchain"

# apt first, as its own layer: it is the large one, and docker pulls layers in
# parallel. The lists are dropped afterwards; install-deps.sh runs its own
# `apt-get update` whenever it runs again.
COPY .github/actions/linux-container-setup/install-deps.sh /tmp/install-deps.sh
RUN bash /tmp/install-deps.sh \
    && mkdir -p /etc/gawk-desktop-ci \
    && sha256sum /tmp/install-deps.sh | cut -d' ' -f1 > /etc/gawk-desktop-ci/install-deps.sha256 \
    && rm -rf /var/lib/apt/lists/* /tmp/install-deps.sh

# Rust under /opt, not $HOME: a container job's HOME is the runner's
# /github/home, a mount over whatever the image had there. CARGO_HOME is only
# set for the install, which is what puts the proxies in /opt/cargo/bin; the
# jobs keep the default registry location (action.yml says why).
ENV RUSTUP_HOME=/opt/rustup
ENV PATH=/opt/cargo/bin:$PATH
COPY gawk-broadcast-desktop/rust-toolchain.toml /tmp/toolchain/rust-toolchain.toml
RUN curl -fsSL https://sh.rustup.rs \
      | CARGO_HOME=/opt/cargo sh -s -- -y --profile minimal --default-toolchain none --no-modify-path \
    && cd /tmp/toolchain \
    && CARGO_HOME=/opt/cargo rustup toolchain install \
    && CARGO_HOME=/opt/cargo rustup component add llvm-tools-preview \
    && CARGO_HOME=/opt/cargo rustup default "$(rustup show active-toolchain | cut -d' ' -f1)" \
    && rustc --version \
    && rm -rf /tmp/toolchain /opt/rustup/downloads /opt/rustup/tmp
