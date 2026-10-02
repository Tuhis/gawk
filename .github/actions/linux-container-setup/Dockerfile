# ghcr.io/tuhis/gawk-desktop-ci: the Linux toolchain broadcast-desktop.yml's
# `lint-host`, `test` and `linux` jobs build and test with (docs/58 D12),
# in two forms, one per stage below. Both are ubuntu:24.04 with this
# action's install-deps.sh and the toolchain rust-toolchain.toml pins already
# applied — the 61-110 s each of those jobs spent on apt and rustup, every
# run, on runners whose image store dies with the pod.
#
# Both are Ubuntu 24.04, so the Linux binary's glibc floor is what it was
# (OD14), and both are separate from the general runner image
# (ghcr.io/tuhis/actions-runner, built in the ioio repo), which D12 keeps out
# of the Linux build on purpose.
#
# Built from the repository root by .github/workflows/desktop-ci-image.yml.
# action.yml says why a stale copy is harmless.

# `ci` (tags `latest`, `<sha>`): the CONTAINER image. A job names it in
# `container:` and runs inside it on the ordinary ioio-k8s pool, which pulls
# it into each pod's fresh, RAM-backed dind store — 29-62 s of "Initialize
# containers" per job. Still the path for forks and hosted runners, and the
# rollback.
FROM ubuntu:24.04 AS ci

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

ENV RUSTUP_HOME=/opt/rustup
ENV PATH=/opt/cargo/bin:$PATH
COPY .github/actions/linux-container-setup/install-toolchain.sh /tmp/install-toolchain.sh
COPY gawk-broadcast-desktop/rust-toolchain.toml /tmp/toolchain/rust-toolchain.toml
RUN bash /tmp/install-toolchain.sh /tmp/toolchain \
    && rm -rf /tmp/toolchain /tmp/install-toolchain.sh

# `runner` (tags `runner-latest`, `runner-<UTC timestamp>`): the same
# toolchain on the official actions-runner image, so that it IS the runner.
# The ioio-k8s-desktop scale set (ioio clusters/baremetal/actions-runners/
# gawk-desktop.yaml) runs it as the runner pod's own container: kubelet keeps
# it on the node's disk between pods, so a job starts with the toolchain
# already there instead of unpacking it into dind. The jobs run on it with no
# `container:` at all (broadcast-desktop.yml, RUNNER_LABEL_DESKTOP).
#
# actions-runner is Ubuntu 24.04 (glibc 2.39), the `ci` stage's own base, and
# the `linux` job's glibc-floor check still asks the shipped binary rather
# than trusting either. The tags live in this already-public package rather
# than a new one: GHCR creates a package private and has no API to flip it,
# and the nodes pull without credentials.
FROM ghcr.io/actions/actions-runner:latest AS runner

LABEL org.opencontainers.image.source="https://github.com/Tuhis/gawk"
LABEL org.opencontainers.image.description="gawk-broadcast-desktop Linux CI runner: actions-runner (Ubuntu 24.04) + GStreamer/PipeWire build and test deps + the pinned Rust toolchain"

USER root
COPY .github/actions/linux-container-setup/install-deps.sh /tmp/install-deps.sh
# sudo: the image runs as `runner`, and action.yml re-runs install-deps.sh
# (apt) when a PR changes it. The base image has the group but not the
# binary.
RUN bash /tmp/install-deps.sh \
    && apt-get install -y --no-install-recommends sudo \
    && echo "runner ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /etc/gawk-desktop-ci \
    && sha256sum /tmp/install-deps.sh | cut -d' ' -f1 > /etc/gawk-desktop-ci/install-deps.sha256 \
    && rm -rf /var/lib/apt/lists/* /tmp/install-deps.sh

ENV RUSTUP_HOME=/opt/rustup
ENV PATH=/opt/cargo/bin:$PATH
COPY .github/actions/linux-container-setup/install-toolchain.sh /tmp/install-toolchain.sh
COPY gawk-broadcast-desktop/rust-toolchain.toml /tmp/toolchain/rust-toolchain.toml
# Writable by `runner`: a PR that bumps rust-toolchain.toml installs the new
# toolchain at job time (action.yml), into this same RUSTUP_HOME.
RUN bash /tmp/install-toolchain.sh /tmp/toolchain \
    && rm -rf /tmp/toolchain /tmp/install-toolchain.sh \
    && chown -R runner:runner /opt/rustup /opt/cargo

USER runner
