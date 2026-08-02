# Developing gawk-telemetry

Dev workflows for the dashboard UI and the build facts that are easy to
get wrong. User-facing docs: the [README](../README.md) and
[`dashboard.md`](dashboard.md).

## Iterating on the dashboard

The dashboard is a **React SPA** (`ui/` — Vite + TypeScript + React +
Zustand, the same toolchain as `gawk-app`) whose build output is embedded
into the binary. The constraint that matters is asserted by a test:
**nothing is fetched from another origin**, so the page works on a
port-forward from a laptop with no network.

Do not iterate through commit → PR → release → deploy. Point the dev
server at a *real* backend and you get hot reload against live broadcasts:

```sh
# 1. forward the deployed read listener
kubectl -n production port-forward svc/gawk-telemetry-read 8081:8081 &

# 2. dev server, proxying /live, /v1 and /mcp to it. The proxy injects the
#    basic-auth header so the browser never prompts and no credential is
#    typed into a page that is being hot-reloaded.
cd ui
npm ci
GAWK_TM_AUTH="admin:$(kubectl -n production get secret gawk-fleet \
  -o jsonpath='{.data.telemetryReadPassword}' | base64 -d)" npm run dev
```

Vite binds IPv6 first, so use **`http://localhost:5174`** — `127.0.0.1`
will be refused.

Prefer a self-contained backend? Run one locally and point it at a
forwarded relay's ops port; you get real relay-side rows, but no
client-side ones (real browsers report to the deployed ingest, not to
yours):

```sh
kubectl -n production port-forward \
  pod/$(kubectl -n production get pod -l app.kubernetes.io/name=gawk-server \
        -o jsonpath='{.items[0].metadata.name}') 12112:2112 &

go run ./cmd/gawk-telemetry \
  -telemetry-key $(printf '0%.0s' {1..64}) \
  -stats-key     $(printf 'ab%.0s' {1..32}) \
  -data-dir /tmp/gawk-tm-dev \
  -ingest-addr 127.0.0.1:18080 -read-addr 127.0.0.1:18081 \
  -relay-addrs 127.0.0.1:12112 -scrape-interval 2s
```

`-relay-addrs` bypasses the headless-Service lookup, which only resolves
in-cluster. With `-stats-key` set the find-a-stream box appears; any 32
bytes work locally as long as you compute digests with the same one:

```sh
curl -sX POST -H 'Content-Type: application/json' \
     -d '{"code":"ABC234"}' http://127.0.0.1:18081/v1/resolve
# {"broadcastKey":"69a445b44f18"}   <- give a card this key to test highlighting
```

## Building the bundle

```sh
cd ui && npm ci && npm run build     # -> ../internal/dashboard/dist
```

**`go build` never depends on npm.** `dist/` carries one committed file so
`//go:embed dist` always compiles; a binary built without the bundle
serves a short "UI not built" page and its API and MCP endpoints are
unaffected. CI builds and tests the UI in its own job, and the release
image builds it in its own Docker stage — nothing generated is committed.

Asset filenames are **stable, not content-hashed**: the page is served
`no-store` (an ops page showing yesterday's bundle after a redeploy is a
page that lies about what it is measuring), which removes the only thing
hashing would buy, and stable names mean a rebuild overwrites in place.

## Build facts that bite

- **The build tag is the whole of the cgo story.** `internal/sqlengine`
  has two implementations behind `//go:build duckdb`; everything else in
  the module is cgo-free and stays that way. The image is
  `CGO_ENABLED=1 -tags duckdb` onto `distroless/cc-debian13`, built by
  `golang:1.26-trixie`, and CI builds both configurations — a build tag
  nobody exercises is a build tag that rots.
- **The base image and the builder are pinned together, and both
  matter.** `cc` rather than `base` because DuckDB is C++ and the binary
  links libstdc++; the same Debian release on both sides because a cgo
  binary links against the *builder's* glibc, which must not be newer than
  the runtime's. Neither failure is visible at build time — both are
  dynamic-linker failures — so CI starts every image it builds and makes
  it answer a request. If you bump one side, bump the other.
- **The image builds from the repo root** — this module consumes
  `gawk-server/wire` through a local `replace`:
  `docker build -f gawk-telemetry/deploy/Dockerfile -t gawk-telemetry:dev ..`

## Traps

- **Chrome throttles a background tab's timers to ~1/min.** A dashboard
  left in another tab stops updating, and a poll-driven test appears to
  hang. It is also why a backgrounded *viewer* reports `renderedFps: 0`
  while decoding fine — the timeline makes that visible rather than
  mysterious. The page marks the gap it did not observe on return, and
  never backfills or draws through it; it is also why the watch feature is
  in-tab only, since a notification could arrive a minute late.
- **The layout must not move while values tick.** The table is
  `table-layout: fixed` with a percentage colgroup, every number is
  `tabular-nums` at a fixed decimal count, and every optional element has
  a reserved slot. A `ch`-based colgroup summed past the container once
  and pushed the last column off-screen; percentages are what guarantee it
  fits. The virtualized lists follow the same rule with a fixed row height
  and a shared grid template, since a `<tr>` cannot be absolutely
  positioned.
- **`npm run build` is the only real typecheck.** The root tsconfig is
  solution-style, so a bare `tsc --noEmit` passes *vacuously*, and vitest
  strips types rather than checking them.
- **The no-external-fetch tests SKIP unless the bundle is built.**
  `TestNoExternalAssetReferences` / `TestServesTheEmbeddedPage` live in
  `internal/dashboard/` but assert against `dist/`, which the Go job never
  builds — running `go test ./internal/dashboard/` locally without
  `npm run build` first shows green while proving nothing. It is the test
  that guards the bundled-ECharts decision, so run it deliberately after
  touching anything under `ui/src/charts/`.
