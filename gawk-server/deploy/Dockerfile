# gawk-server relay image. Build context is gawk-server/ (the Go module
# root), the Dockerfile lives one level down:
#
#   docker build -f deploy/Dockerfile -t gawk-server:dev .
#
# gawk-echo ships alongside the server on purpose: the server has no TCP
# listener and kubelet can't speak HTTP/3, so the Helm chart's probes exec
# gawk-echo against localhost — and it doubles as an in-container
# connectivity diagnostic.

# `-trixie`, not the bare `1.26`. This is a no-op for what gets built: on
# linux/amd64 both tags resolve to the SAME manifest,
# sha256:dbb10bd1b3400ba0858e2f7c354fd4556b782c187feeff52789d4ee156a84db8. The
# bare tag's index merely carries two extra windows/amd64 entries on top of the
# Linux set, which nothing here can use.
#
# Naming the Debian release is the point — the same explicitness gawk-telemetry's
# Dockerfile applies to its own builder, though there it is load-bearing for a
# different reason (a cgo binary must not link a newer glibc than its runtime
# carries). This binary is static and cgo-free, so that argument does not apply
# here; this is just a pin that says what it means.
#
# It was originally made for a third reason that no longer holds, and the note
# is kept because the failure was expensive: the CI runners resolve base images
# through an in-cluster pull-through cache (ioio
# clusters/baremetal/actions-runners/registry-cache.yaml), and that cache could
# not sync the bare tag — its two Windows entries are Docker-format manifests,
# which zot rejected until it was configured with http.compat=["docker2s2"]. A
# tag the cache cannot serve is not a neutral miss, because buildkit then falls
# back to Docker Hub directly and that path is pathologically slow here:
#
#   #3 [internal] load metadata for docker.io/library/golang:1.26   465.0s
#
# 465 of that job's 504 seconds (run 30762746378), for one manifest. The cache
# handles both tags now, so this pin is no longer what makes the build fast —
# but if a base image ever resolves slowly again, that is the shape to look for.
FROM golang:1.26-trixie AS build
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
# The MIT/BSD/ISC attribution obligations attach to the distributed binary,
# so the notices ride in the image, not just the repo. Via the build stage:
# the repo-root LICENSE is outside this module-dir build context.
COPY --from=build /src/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
EXPOSE 4433/udp
ENTRYPOINT ["/gawk-server"]
