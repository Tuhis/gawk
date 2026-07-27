# gawk-telemetry image (R28, docs/33). The build context is the REPO ROOT, not
# this module's directory: gawk-telemetry consumes gawk-server/wire through a
# local `replace` (D12, following R14 Decision 11), so the relay module's tree
# has to be present at build time.
#
#   docker build -f gawk-telemetry/deploy/Dockerfile -t gawk-telemetry:dev .
#
# Nothing cgo is involved: DuckDB is a query OPTION, not a runtime dependency
# (D11), which is exactly what keeps this a static distroless image.

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
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-telemetry ./cmd/gawk-telemetry

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gawk-telemetry /gawk-telemetry
# 8080 is the PUBLIC ingest listener; 8081 is the read listener (dashboard,
# read API, MCP) and must never be routed publicly — D14.
EXPOSE 8080
EXPOSE 8081
ENTRYPOINT ["/gawk-telemetry"]
