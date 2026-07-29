# gawk-telemetry image (R28, docs/33). The build context is the REPO ROOT, not
# this module's directory: gawk-telemetry consumes gawk-server/wire through a
# local `replace` (D12, following R14 Decision 11), so the relay module's tree
# has to be present at build time.
#
#   docker build -f gawk-telemetry/deploy/Dockerfile -t gawk-telemetry:dev .
#
# **This image is cgo, and deliberately so** (R31 UD18, docs/36 §8 Q1). The
# owner's decision was a SQL console on by default; every usable Go DuckDB
# driver is cgo, so the deployed image pays for it — `-tags duckdb`,
# CGO_ENABLED=1, and `distroless/base` instead of `distroless/static` because
# the binary now needs a libc.
#
# What did NOT change is the property that decision threatened: `go build ./...`
# on a fresh clone is still cgo-free and still needs no C toolchain. The engine
# lives behind the build tag, and a binary without it says plainly that this
# deployment has no engine rather than rendering a broken editor. So the cost is
# paid once, here, by the thing that is actually deployed.

# The dashboard is a React SPA (ui/) whose build output is embedded into the Go
# binary. It is built FIRST and in its own stage so the Go build never depends
# on npm being present — the same split that keeps `go build ./...` working on a
# fresh clone, where the binary simply serves a "UI not built" page.
#
# Only the manifests are copied before `npm ci`, so a source-only change reuses
# the dependency layer.
FROM node:26-alpine AS ui
WORKDIR /ui
COPY gawk-telemetry/ui/package.json gawk-telemetry/ui/package-lock.json ./
RUN npm ci
COPY gawk-telemetry/ui/ ./
# vite.config.ts writes to ../internal/dashboard/dist, so give it that shape.
RUN mkdir -p /internal/dashboard && \
    sed -i "s#'../internal/dashboard/dist'#'/internal/dashboard/dist'#" vite.config.ts && \
    npm run build

FROM golang:1.26 AS build
WORKDIR /src
COPY gawk-server/ ./gawk-server/
COPY gawk-telemetry/ ./gawk-telemetry/
# The bundle lands where //go:embed dist expects it, replacing the committed
# placeholder directory.
COPY --from=ui /internal/dashboard/dist/ ./gawk-telemetry/internal/dashboard/dist/
WORKDIR /src/gawk-telemetry
RUN go mod download
ARG VERSION=dev
# -tags duckdb selects internal/sqlengine's real engine (the untagged build gets
# the stub). CGO_ENABLED=1 follows from the driver, not from a preference.
RUN CGO_ENABLED=1 go build -tags duckdb -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-telemetry ./cmd/gawk-telemetry

# `base`, not `static`: a cgo binary needs a libc. Still distroless, still
# nonroot, still no shell.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/gawk-telemetry /gawk-telemetry
# 8080 is the PUBLIC ingest listener; 8081 is the read listener (dashboard,
# read API, MCP) and must never be routed publicly — D14.
EXPOSE 8080
EXPOSE 8081
ENTRYPOINT ["/gawk-telemetry"]
