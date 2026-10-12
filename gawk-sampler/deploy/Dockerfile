# gawk-sampler image (R40, docs/71 D4/D5). The build context is the REPO ROOT,
# not this module's directory: gawk-sampler consumes gawk-server/screening
# through a local `replace` (D3 — one definition of the contract, never a
# mirror), so the relay module's tree has to be present at build time. Same
# constraint the gawk-admin and gawk-telemetry images document.
#
#   docker build -f gawk-sampler/deploy/Dockerfile -t gawk-sampler:dev .
#   docker build -f gawk-sampler/deploy/Dockerfile --target test .           # the suite, natively
#   docker build -f gawk-sampler/deploy/Dockerfile --target runtime-check .  # …and on distroless
#
# Three things travel in it besides the binary, and one deliberately does not:
#
#   * a MINIMAL ffmpeg (build-ffmpeg.sh): the h264/vp8/vp9 decoders and the
#     pipe from a keyframe to one RGB frame, nothing else. The sampler runs it
#     as a SUBPROCESS per sample (D4) — a decoder crash or CVE costs one
#     sample, never the sampler's address space — so it is never linked, and
#     it is LGPL-2.1-or-later only (no --enable-gpl, no --enable-nonfree). Its
#     licence text and the exact source and configure line ship under
#     /licenses/ffmpeg (docs/71 §8.1).
#   * ONNX Runtime's shared library, which the sampler loads at start-up
#     through onnxruntime_go (cgo, dlopen). MIT; its LICENSE and
#     ThirdPartyNotices ship under /licenses/onnxruntime.
#   * NOT the model. NudeNet's weights are AGPL-3.0 and gawk never
#     redistributes them: the chart's init container fetches them at deploy
#     time and checks a pinned SHA-256 (D5, §8.1). An image with no model in it
#     is also what lets CI and the dev stack run a generated test model.
#
# **This image is cgo** (onnxruntime_go), so the gawk-telemetry header's two
# lessons apply: the runtime is `cc` (ONNX Runtime is C++ and needs libstdc++),
# and the builder and the runtime are pinned to the SAME Debian release
# (trixie / debian13) so the binary, ffmpeg and the runtime agree on glibc. CI
# runs the built image (the `sampler` job), because building one is not
# evidence that it starts.

# ── ffmpeg ────────────────────────────────────────────────────────────────
# Built from the pinned, SHA-checked release tarball by the module's own
# script, which a developer also runs to get the ffmpeg the native tests use.
# nasm buys the decoders their SIMD paths (the script builds without it, just
# slower per sample). Only the script is copied first, so a Go-only change
# reuses this layer — the slowest one in the build.
FROM golang:1.26-trixie AS ffmpeg
RUN apt-get update \
 && apt-get install -y --no-install-recommends nasm xz-utils \
 && rm -rf /var/lib/apt/lists/*
COPY gawk-sampler/deploy/build-ffmpeg.sh /usr/local/bin/build-ffmpeg.sh
RUN sh /usr/local/bin/build-ffmpeg.sh /opt/ffmpeg /tmp/ffmpeg-build \
 && rm -rf /tmp/ffmpeg-build

# ── ONNX Runtime ──────────────────────────────────────────────────────────
# The release tarball, pinned by version AND SHA-256 (GitHub's published asset
# digest). The version is the one onnxruntime_go's C API headers are written
# against (its README: "this library uses version 1.29.0 of the onnxruntime C
# API headers"); bump the two together.
#
# The execution provider is a BUILD ARGUMENT (docs/71 §3, Rejected: a GPU image
# variant): the CPU package by default; a GPU variant sets ORT_PACKAGE to a
# CUDA package and ORT_SHA256 to its digest — and needs a CUDA runtime base
# instead of distroless, which is why it is not a variant today.
FROM golang:1.26-trixie AS onnxruntime
ARG ORT_VERSION=1.29.0
ARG ORT_PACKAGE=onnxruntime-linux-x64
ARG ORT_SHA256=c3fddc4f139a045b0c4902c57410f0694f1c2fdf9b6939fbe38b1aeae7cd14ba
WORKDIR /tmp/ort
RUN curl -fsSL -o ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/${ORT_PACKAGE}-${ORT_VERSION}.tgz" \
 && echo "${ORT_SHA256}  ort.tgz" | sha256sum -c - \
 && tar -xzf ort.tgz \
 && mkdir -p /out/lib /out/licenses \
 # -L: the tarball ships libonnxruntime.so as a symlink chain; the image
 # carries the one real file, loaded by its full path (-onnxruntime-lib).
 && cp -L "${ORT_PACKAGE}-${ORT_VERSION}/lib/libonnxruntime.so" /out/lib/libonnxruntime.so \
 && cp "${ORT_PACKAGE}-${ORT_VERSION}/LICENSE" "${ORT_PACKAGE}-${ORT_VERSION}/ThirdPartyNotices.txt" \
       "${ORT_PACKAGE}-${ORT_VERSION}/VERSION_NUMBER" /out/licenses/

# ── the sampler ───────────────────────────────────────────────────────────
FROM golang:1.26-trixie AS build
WORKDIR /src
# Manifests before sources: the `go mod download` layer then depends on the
# go.mod/go.sum files alone and survives every edit that does not touch
# dependencies. gawk-sampler `replace`s gawk-server with ../gawk-server, so
# that module's manifests are part of its graph.
COPY gawk-server/go.mod gawk-server/go.sum ./gawk-server/
COPY gawk-sampler/go.mod gawk-sampler/go.sum ./gawk-sampler/
WORKDIR /src/gawk-sampler
RUN go mod download
WORKDIR /src
COPY gawk-server/ ./gawk-server/
COPY gawk-sampler/ ./gawk-sampler/
WORKDIR /src/gawk-sampler
ARG VERSION=dev
# CGO_ENABLED=1 follows from onnxruntime_go, not from a preference: it binds
# the C API and dlopens the library the runtime stage carries.
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-sampler ./cmd/gawk-sampler

# ── the suite, against the real ffmpeg and ONNX Runtime ───────────────────
# `docker build --target test` is what CI's `sampler` job runs: the native
# tests (bitstream conversion through the real ffmpeg, the decode timeout and
# RLIMIT_CORE, inference on both test models) skip without these binaries on
# a developer's machine, and GAWK_SAMPLER_REQUIRE_NATIVE turns that skip into
# a failure here — a suite that skipped its native half would prove nothing.
# The ffmpeg and the library are the SAME files the runtime stage copies.
FROM build AS test
COPY --from=ffmpeg /opt/ffmpeg/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=onnxruntime /out/lib/libonnxruntime.so /usr/lib/libonnxruntime.so
ENV CGO_ENABLED=1 \
    GAWK_SAMPLER_FFMPEG=/usr/local/bin/ffmpeg \
    GAWK_SAMPLER_ONNXRUNTIME_LIB=/usr/lib/libonnxruntime.so \
    GAWK_SAMPLER_REQUIRE_NATIVE=1
RUN test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
RUN go vet ./...
RUN go test -race -count=1 ./...

# ── the native tests again, where the files will actually run ─────────────
# The test stage runs on the Go builder, which has a full Debian userland; the
# runtime is distroless, which has almost none. A library ffmpeg or ONNX
# Runtime needs and only the builder carries would pass every test above and
# fail in the cluster — the R31 shape. So the decode and inference tests are
# compiled here and RUN on the runtime's base with the runtime's exact files:
# the shipped ffmpeg decoding the H.264, VP8 and VP9 fixtures through the code
# that builds the D4 command line, and the shipped library loading both test
# models. `docker build --target runtime-check` is what CI runs; nothing is
# exported from it.
FROM build AS check-bin
RUN CGO_ENABLED=1 go test -c -o /out/check/decode.test ./internal/decode \
 && CGO_ENABLED=1 go test -c -o /out/check/detect.test ./internal/detect

# The three COPYs below must stay the runtime stage's (same sources, same
# destinations) — that equality is the whole point of this stage.
FROM gcr.io/distroless/cc-debian13:nonroot AS runtime-check
COPY --from=ffmpeg /opt/ffmpeg/bin/ffmpeg /usr/bin/ffmpeg
COPY --from=onnxruntime /out/lib/libonnxruntime.so /usr/lib/libonnxruntime.so
COPY --from=check-bin /out/check/ /check/
# The tests find testdata relative to their compiled-in source path (/src/…).
COPY gawk-sampler/testdata/ /src/gawk-sampler/testdata/
ENV GAWK_SAMPLER_FFMPEG=/usr/bin/ffmpeg \
    GAWK_SAMPLER_ONNXRUNTIME_LIB=/usr/lib/libonnxruntime.so \
    GAWK_SAMPLER_REQUIRE_NATIVE=1
# Exec form: there is no shell here. The three skipped tests stand in for
# ffmpeg with /bin/sh scripts, so they cannot run on distroless at all (the
# test stage ran them); skipping BY NAME means a new shell-using test fails
# here loudly instead of being skipped quietly.
RUN ["/check/decode.test", "-test.v", "-test.skip", "^(TestDecodeKillsAHangingDecoder|TestDecodeReportsAFailingDecoder|TestDecodeGivesTheDecoderNoEnvironment)$"]
RUN ["/check/detect.test", "-test.v"]

# ── runtime ───────────────────────────────────────────────────────────────
# `cc`, not `static`: ONNX Runtime needs libstdc++ and libgcc_s, and the
# binary and ffmpeg link glibc. `debian13` to match the trixie builders. Still
# distroless, still nonroot (uid 65532), still no shell — the chart's init
# container runs `fetch-model` through the same exec-form ENTRYPOINT. Last in
# the file, so a plain `docker build` (the release publish) builds this stage.
FROM gcr.io/distroless/cc-debian13:nonroot
COPY --from=ffmpeg /opt/ffmpeg/bin/ffmpeg /usr/bin/ffmpeg
COPY --from=onnxruntime /out/lib/libonnxruntime.so /usr/lib/libonnxruntime.so
COPY --from=build /out/gawk-sampler /gawk-sampler
# Attribution ships with what it covers: the repository LICENSE, the Go
# notices for the binary, and the two native components' own terms.
COPY LICENSE /LICENSE
COPY gawk-sampler/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
COPY --from=ffmpeg /opt/ffmpeg/share/doc/ffmpeg/ /licenses/ffmpeg/
COPY --from=onnxruntime /out/licenses/ /licenses/onnxruntime/
# /metrics, /healthz and /readyz — ClusterIP only, never routed.
EXPOSE 9090
ENTRYPOINT ["/gawk-sampler"]
