# gawk-mumble-bridge image (R79, docs/80 D1). The build context is the REPO
# ROOT, not this module's directory: the bridge consumes gawk-server/wire and
# gawk-server/voice through a local `replace` (one definition of the
# contract, never a mirror), so the relay module's tree has to be present at
# build time — the gawk-sampler and gawk-admin images' constraint.
#
#   docker build -f gawk-mumble-bridge/deploy/Dockerfile -t gawk-mumble-bridge:dev .
#   docker build -f gawk-mumble-bridge/deploy/Dockerfile --target test .   # gofmt, vet, the suite
#
# The bridge is pure Go (CGO_ENABLED=0): no libopus — Opus passes through
# untouched (docs/80 OD26) — so the runtime is distroless `static`.
#
# It builds against STOCK quic-go at the version gawk-server requires, not
# the relay's vendored, patched copy (gawk-server/third_party/quic-go): a
# `replace` applies only in the main module, and the copy's patches (a
# 512-deep datagram queue, GSO batching) are for the relay's video bursts.
# Voice is one ~50-byte datagram per speaker per 10 ms.

FROM golang:1.27-trixie AS build
WORKDIR /src
# Manifests first: the `go mod download` layer depends on them alone.
COPY gawk-server/go.mod gawk-server/go.sum ./gawk-server/
COPY gawk-mumble-bridge/go.mod gawk-mumble-bridge/go.sum ./gawk-mumble-bridge/
WORKDIR /src/gawk-mumble-bridge
RUN go mod download
WORKDIR /src
COPY gawk-server/ ./gawk-server/
COPY gawk-mumble-bridge/ ./gawk-mumble-bridge/
WORKDIR /src/gawk-mumble-bridge
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-mumble-bridge ./cmd/gawk-mumble-bridge \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/gawk-mumblesim ./cmd/gawk-mumblesim

# `docker build --target test`: the unit suite (the murmur integration tests
# skip here: they need GAWK_TEST_MURMUR and a murmur, testdata/murmur.sh).
FROM build AS test
RUN test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# The e2e stand-in for a native Mumble user (docs/80 MV7), as its own small
# image target: `--target mumblesim`.
FROM gcr.io/distroless/static-debian13:nonroot AS mumblesim
COPY --from=build /out/gawk-mumblesim /gawk-mumblesim
COPY gawk-mumble-bridge/testdata/tone-440hz-10ms.opus /tone-440hz-10ms.opus
ENTRYPOINT ["/gawk-mumblesim"]

# ── runtime ───────────────────────────────────────────────────────────────
# Last in the file, so a plain `docker build` (the release publish) builds
# it. Distroless, nonroot (uid 65532), no shell.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gawk-mumble-bridge /gawk-mumble-bridge
# Attribution ships with what it covers. THIRD-PARTY-NOTICES.md is generated
# by tools/licenses/gen-notices.py and kept fresh by the licenses-fresh job.
COPY LICENSE /LICENSE
COPY gawk-mumble-bridge/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
# 4443/udp: browsers' voice sessions (public, a UDP LoadBalancer).
# 4444/udp: the relay's observer links (internal only).
# 2113/tcp: /healthz, /readyz, /metrics, /statusz (internal only).
EXPOSE 4443/udp 4444/udp 2113/tcp
ENTRYPOINT ["/gawk-mumble-bridge"]
