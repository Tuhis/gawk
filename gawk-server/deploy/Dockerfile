# gawk-server relay image. Build context is gawk-server/ (the Go module
# root), the Dockerfile lives one level down:
#
#   docker build -f deploy/Dockerfile -t gawk-server:dev .
#
# gawk-echo ships alongside the server on purpose: the server has no TCP
# listener and kubelet can't speak HTTP/3, so the Helm chart's probes exec
# gawk-echo against localhost — and it doubles as an in-container
# connectivity diagnostic.

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION is stamped into the binary (main.version); CI passes the release
# version, local builds get "dev". CGO is off for the static/distroless
# runtime image — the -race test suite needs CGO and runs outside the image
# build (see README).
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-server ./cmd/gawk-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/gawk-echo ./cmd/gawk-echo

# :nonroot runs as uid 65532 — no USER line needed. No shell exists in this
# image: ENTRYPOINT must stay exec-form and probes must exec binaries.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gawk-server /gawk-server
COPY --from=build /out/gawk-echo /gawk-echo
EXPOSE 4433/udp
ENTRYPOINT ["/gawk-server"]
