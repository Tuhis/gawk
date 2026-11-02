# gawk-whip-gateway image (R33, docs/83 §6). The build context is the REPO
# ROOT, not this module's directory: the gateway compiles gawk-server/publisher
# and gawk-server/wire through a local `replace` (one publisher engine and one
# wire format, never a mirror), so the relay module's tree has to be present at
# build time, as for the gawk-mumble-bridge, gawk-sampler and gawk-admin images.
#
#   docker build -f gawk-whip-gateway/deploy/Dockerfile -t gawk-whip-gateway:dev .
#   docker build -f gawk-whip-gateway/deploy/Dockerfile --target test .     # gofmt, vet, the suite
#   docker build -f gawk-whip-gateway/deploy/Dockerfile --target whipsim .  # the dev stack's sender
#
# The gateway is pure Go (CGO_ENABLED=0): pion does ICE, DTLS and SRTP in Go,
# and the gateway never decodes or encodes media, so the runtime is
# distroless `static`.
#
# It builds against STOCK quic-go at the version gawk-server requires, not the
# relay's vendored, patched copy (gawk-server/third_party/quic-go): a `replace`
# applies only in the main module, and the copy's patches are for the relay's
# fan-out. The gateway is one publisher per OBS session.

FROM golang:1.27-trixie AS build
WORKDIR /src
# Manifests first: the `go mod download` layer depends on them alone.
COPY gawk-server/go.mod gawk-server/go.sum ./gawk-server/
COPY gawk-whip-gateway/go.mod gawk-whip-gateway/go.sum ./gawk-whip-gateway/
WORKDIR /src/gawk-whip-gateway
RUN go mod download
WORKDIR /src
COPY gawk-server/ ./gawk-server/
COPY gawk-whip-gateway/ ./gawk-whip-gateway/
WORKDIR /src/gawk-whip-gateway
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-whip-gateway ./cmd/gawk-whip-gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/gawk-whipsim ./cmd/gawk-whipsim

# `docker build --target test`: the suite. The end-to-end tests `go build`
# the relay from ../gawk-server, which is in the build context, so they run
# here too.
FROM build AS test
RUN test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# The dev stack's WHIP sender (docs/83 WG9, docker-compose.yml's `whip`
# profile), as its own image target: `--target whipsim`. gawk-whipsim mints
# a stream key and drives FFmpeg's WHIP muxer (FFmpeg 8 or later) with a
# test pattern. Alpine, because the sender needs a real H.264 encoder and a
# WHIP client, and Alpine's ffmpeg package ships both. Test tooling only: it
# never rides in the gateway image.
FROM alpine:3.24 AS whipsim
RUN apk add --no-cache ffmpeg
COPY --from=build /out/gawk-whipsim /usr/local/bin/gawk-whipsim
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/gawk-whipsim"]

# ── runtime ───────────────────────────────────────────────────────────────
# Last in the file, so a plain `docker build` (the release publish) builds
# it. Distroless, nonroot (uid 65532), no shell.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/gawk-whip-gateway /gawk-whip-gateway
# Attribution ships with what it covers. THIRD-PARTY-NOTICES.md is generated
# by tools/licenses/gen-notices.py and kept fresh by the licenses-fresh job.
COPY LICENSE /LICENSE
COPY gawk-whip-gateway/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
# 8080/tcp: WHIP (POST/DELETE/OPTIONS /whip…, /whip/status), behind the
#           public Ingress, which terminates TLS.
# 8081/tcp: the peer lookup between gateway pods (D10). Internal only.
# 9090/tcp: /healthz, /readyz, /metrics. Internal only.
# 8189+N/udp and /tcp: media (ICE over UDP, passive ICE-TCP) on
#           -media-base-port + the pod's ordinal, one LoadBalancer per pod.
EXPOSE 8080/tcp 8081/tcp 9090/tcp 8189/udp 8189/tcp
ENTRYPOINT ["/gawk-whip-gateway"]
