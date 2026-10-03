# gawk-admin image (R39, docs/42 §4.13). The build context is the REPO ROOT,
# not this module's directory: gawk-admin consumes gawk-server/moderation
# through a local `replace` (D13 — the relay and the admin plane must agree
# byte-for-byte on normalization, target matching and expiry semantics), so the
# relay module's tree has to be present at build time. Same constraint
# gawk-telemetry's image already documents.
#
#   docker build -f gawk-admin/deploy/Dockerfile -t gawk-admin:dev .
#
# Unlike gawk-telemetry this image is NOT cgo: the whole dependency set —
# pgx, client-go, golang-migrate, go-oidc — is pure Go, so the static
# distroless runtime applies and the builder/runtime glibc pairing that image's
# header warns about is simply not a constraint here.
#
# ONE binary with two modes. `gawk-admin migrate` applies the schema and exits;
# the default mode serves the portal and /api/v1. The Helm pre-install/
# pre-upgrade hook Job runs the FIRST from the SAME image the Deployment is
# about to roll (docs/42 §4.15/D18) — that is the whole reason the migrations
# are embedded and the subcommand is not a second artifact: the schema a
# release needs travels inside that release.

# The portal is a React SPA (ui/) whose build output is embedded into the Go
# binary. It is built FIRST and in its own stage so the Go build never depends
# on npm being present — the same split that keeps `go build ./...` working on
# a fresh clone, where the binary serves a "UI not built" page instead.
#
# Only the manifests are copied before `npm ci`, so a source-only change reuses
# the dependency layer.
#
# The stage mirrors the repository layout under /src (docs/55 D3): the UI
# depends on the shared session package as `file:../../common-ts/oidc-session`,
# and `npm ci` links that path, so it has to resolve here exactly as it does
# in a checkout. The package's own package.json is one of the manifests — it is
# what `npm ci` reads — and its source follows with the UI's. The same layout
# also puts vite.config.ts's `../internal/portal/dist` where the Go stage
# collects it, with no path rewriting.
FROM node:26-alpine AS ui
WORKDIR /src/gawk-admin/ui
COPY gawk-admin/ui/package.json gawk-admin/ui/package-lock.json ./
COPY common-ts/oidc-session/package.json /src/common-ts/oidc-session/
RUN npm ci
COPY common-ts/oidc-session/ /src/common-ts/oidc-session/
COPY gawk-admin/ui/ ./
RUN npm run build

FROM golang:1.26-trixie AS build
WORKDIR /src
# Manifests before sources: the `go mod download` layer then depends on the
# go.mod/go.sum files alone, and a persistent build cache (a developer's own
# `docker compose up --build`) reuses it across every edit that does not touch
# dependencies, instead of re-downloading every module after each change.
# gawk-admin `replace`s gawk-server with ../gawk-server, so that module's
# manifests are part of its graph.
COPY gawk-server/go.mod gawk-server/go.sum ./gawk-server/
COPY gawk-admin/go.mod gawk-admin/go.sum ./gawk-admin/
WORKDIR /src/gawk-admin
RUN go mod download
WORKDIR /src
COPY gawk-server/ ./gawk-server/
COPY gawk-admin/ ./gawk-admin/
# The bundle lands where //go:embed dist expects it, alongside the committed
# placeholder README.md that keeps the embed compilable on a fresh clone.
COPY --from=ui /src/gawk-admin/internal/portal/dist/ ./gawk-admin/internal/portal/dist/
WORKDIR /src/gawk-admin
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gawk-admin ./cmd/gawk-admin

# static, not cc: no cgo, no C++ runtime, nothing dynamically linked. Still
# distroless, still nonroot (uid 65532), still no shell — ENTRYPOINT stays
# exec-form and the migrate Job overrides `args`, never `command` with a shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gawk-admin /gawk-admin
# Attribution ships with the binary that links it: the Go notices for this
# module and the UI notices for the embedded portal bundle, plus LICENSE
# (reachable because the build context is the repo root).
COPY LICENSE /LICENSE
COPY gawk-admin/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES.md
COPY gawk-admin/ui/THIRD-PARTY-NOTICES.md /THIRD-PARTY-NOTICES-ui.md
# ONE listener (docs/42 §4.6): the SPA, /api/v1, /auth/config, /healthz and
# /readyz all share it. There is no second port to expose and no non-public
# half to keep off an Ingress — the auth boundary here is OIDC, not network
# placement (D7).
EXPOSE 8090
EXPOSE 8091
ENTRYPOINT ["/gawk-admin"]
