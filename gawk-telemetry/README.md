# gawk-telemetry

Optional per-session diagnostics for a gawk relay fleet (R28, [`../docs/33-telemetry-and-diagnostics.md`](../docs/33-telemetry-and-diagnostics.md)).

Every broadcast and every viewer session records what actually happened, into a
store that answers three questions with no human copy-pasting anything: *how is
this stream going right now*, *what was that specific viewer's experience*, and
*how does this compare to the average*.

**It adds no measurement.** `ViewerStats` (~80 fields), `BroadcastStats`,
`engine.Stats` and `RegistryStats` already exist. This is a pipe, a store and a
query surface.

## Off by default

Telemetry is gated by the **relay**, not by this service. With no fleet
telemetry key the relay sends no `TelemetryHello` (wire `0x0D`) and every
client collects nothing — observably identical to a relay predating R28. Three
things have to be enabled together, sharing one key:

```sh
KEY=$(openssl rand -hex 32)
kubectl -n gawk create secret generic gawk-telemetry-key --from-literal=key="$KEY"

# 1. the relay mints session tokens + exposes the headless scrape Service
helm upgrade gawk-server ... \
  --set telemetry.enabled=true \
  --set telemetry.keyRef.name=gawk-telemetry-key --set telemetry.keyRef.key=key

# 2. this service verifies them
helm upgrade --install gawk-telemetry oci://ghcr.io/tuhis/charts/gawk-telemetry -n gawk \
  --set telemetryKeyRef.name=gawk-telemetry-key --set telemetryKeyRef.key=key

# 3. the frontend routes the same-origin ingest path
helm upgrade gawk-app ... --set telemetry.enabled=true
```

An install that leaves all three off renders byte-identically to the pre-R28
charts — asserted in CI, not merely intended.

## Two listeners, and the split is the security posture

| Listener | Default | Carries | Exposure |
|----------|---------|---------|----------|
| ingest | `:8080` | `POST /api/telemetry/v1/ingest` | **Public**, via a same-origin path on the frontend's Ingress |
| read | `:8081` | dashboard, read API, MCP | **Never public** — ClusterIP, port-forward, or an internal Ingress with basic auth |

The read side aggregates every broadcast on the fleet and should be no more
reachable than `/statusz` is today (R9 D1's posture). The chart refuses to
render a read Ingress without basic auth.

```sh
kubectl -n gawk port-forward svc/gawk-telemetry-read 8081:8081
open http://localhost:8081          # the live dashboard
curl localhost:8081/v1/sessions     # the read API
```

## The dashboard

One page listing every live broadcast with its broadcaster **and** each viewer,
with anything obviously wrong highlighted before you click anything. Severity —
not recency — sorts the live group; recently ended broadcasts sit in a separate
recessed group below, carrying their stored verdicts in the past tense.

Four states: **ok · warn · bad · unknown**. A viewer whose telemetry stopped
reads *stale*; one that never reported reads *unknown*. Neither is ever `ok` —
painting an absence of evidence as green is the one thing an ops dashboard must
not do.

No build step, no external asset fetch: it works on a port-forward from a
laptop with no network.

### Finding one stream

Rows are labelled with the **obfuscated** broadcast key, because that is the
only identity telemetry is ever told: the six-character code is a join
credential, and the client is structurally incapable of reporting one (the
session token's HMAC binds the obfuscated key, so a batch carrying anything else
is rejected before a byte is written).

So the lookup runs one-way and server-side. Set `-stats-key` to the fleet stats
key and a **Find** box appears: type the code you already hold, the service
computes the digest the relay would have published for it, and the page
highlights that row. `POST /v1/resolve`, never a query string — a join
credential must not land in browser history, a `Referer`, or a proxy log. The
code is never stored and never logged.

**It is off by default and that is deliberate.** A service holding the stats key
can enumerate join codes for the broadcasts it has stored — 31⁶ HMACs is minutes
of work — so turning it on is an explicit act, the same posture `query_sql`
takes. Leave it unset and the endpoint answers 501 and the box does not render.

## MCP

The read listener serves MCP at `/mcp` (streamable HTTP). Point Claude Code at
it through the same port-forward:

```json
{ "mcpServers": { "gawk": { "type": "http", "url": "http://localhost:8081/mcp" } } }
```

Start with `diagnose(sessionId)` — it runs [docs/13's bottleneck
playbook](../docs/13-observability.md#bottleneck-playbook) and returns **ranked
verdicts with evidence, never raw samples**. A 4-hour session's diagnosis is
under 1 KB; every default response is bounded at 32 KB and a test asserts it.

Each piece of evidence is tagged `relay | client | derived`, and a verdict
resting only on client testimony caps its own confidence — *a wedged client's
own accounting is the least reliable evidence in the system, and it is exactly
what a wedged client sends* (docs/20 finding 7).

## Storage

```
/data/
  sessions/date=2026-07-26/broadcast=<12 hex>/<sessionId>.ndjson[.gz]
  rollups/date=2026-07-26.ndjson          # permanent, one line per session
  relay/date=2026-07-26/<pod>.ndjson[.gz] # scraped relay snapshots
```

Hive-partitioned, so retention is a **directory delete, not a query**, and
DuckDB can read it straight off the PVC:

```sql
SELECT sessionId, stats->>'receivedFps'
FROM read_json_auto('/data/sessions/date=2026-07-2*/**/*.ndjson.gz', hive_partitioning=1)
WHERE kind = 'sample';
```

DuckDB is a **query option, not a runtime dependency** — the service is plain,
cgo-free Go. Raw sessions are kept 14 days; **rollups are permanent**.

Ten viewers × 2 h/day ≈ 5 MB/day, ≈ 75 MB across the whole window.

## Privacy

No IP addresses (the rate limiter uses one and never persists it), no full
user-agent strings (reduced on the device to `"Chrome 152"` / `"Windows"`), no
cross-session or cross-broadcast identity, no fingerprinting, and never any
media. The client never reports the raw joinable broadcast ID — only the
obfuscated key the relay handed it, which the session token's HMAC binds.

## Developing the dashboard

The dashboard is a **React SPA** (`ui/` — Vite + TypeScript + React + Zustand,
the same toolchain as `gawk-app`) whose build output is embedded into this
binary. §4.8.4 originally specified hand-written HTML with no build step; the
amendment and its reasoning are in docs/33. The constraint that mattered is
unchanged and still asserted by a test: **nothing is fetched from another
origin**, so the page works on a port-forward from a laptop with no network.

**Do not iterate on the UI through commit → PR → release → deploy.** Point the
dev server at a *real* backend and you get hot reload against live broadcasts:

```sh
# 1. forward the deployed read listener
kubectl -n production port-forward svc/gawk-telemetry-read 8081:8081 &

# 2. dev server, proxying /live, /v1 and /mcp to it. The proxy injects the
#    basic-auth header so the browser never prompts and no credential is typed
#    into a page that is being hot-reloaded.
cd ui
npm ci
GAWK_TM_AUTH="admin:$(kubectl -n production get secret gawk-fleet \
  -o jsonpath='{.data.telemetryReadPassword}' | base64 -d)" npm run dev
```

Vite binds IPv6 first, so use **`http://localhost:5174`** — `127.0.0.1` will be
refused.

Prefer a self-contained backend? Run one locally and point it at a forwarded
relay's ops port; you get real relay-side rows, but no client-side ones (real
browsers report to the deployed ingest, not to yours):

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
in-cluster. With `-stats-key` set the find-a-stream box appears; any 32 bytes
work locally as long as you compute digests with the same one:

```sh
curl -sX POST -H 'Content-Type: application/json' \
     -d '{"code":"ABC234"}' http://127.0.0.1:18081/v1/resolve
# {"broadcastKey":"69a445b44f18"}   <- give a card this key to test highlighting
```

### Building the bundle

```sh
cd ui && npm ci && npm run build     # -> ../internal/dashboard/dist
```

**`go build` never depends on npm.** `dist/` carries one committed file so
`//go:embed dist` always compiles; a binary built without the bundle serves a
short "UI not built" page and its API and MCP endpoints are unaffected. CI
builds and tests the UI in its own job, and the release image builds it in its
own Docker stage — so nothing generated is committed.

Asset filenames are **stable, not content-hashed**: the page is served
`no-store` (an ops page showing yesterday's bundle after a redeploy is a page
that lies about what it is measuring), which removes the only thing hashing
would buy, and stable names mean a rebuild overwrites in place.

### Two traps worth knowing

- **Chrome throttles a background tab's timers to ~1/min.** A dashboard left in
  another tab stops updating, and a poll-driven test appears to hang. It is also
  why a backgrounded *viewer* reports `renderedFps: 0` while decoding fine — the
  timeline makes that visible rather than mysterious.
- **The layout must not move while values tick.** The table is
  `table-layout: fixed` with a percentage colgroup, every number is
  `tabular-nums` at a fixed decimal count, and every optional element has a
  reserved slot. A `ch`-based colgroup summed past the container once and pushed
  the last column off-screen; percentages are what guarantee it fits.

## Build & test

```sh
go build ./...
go vet ./...
go test -race ./...

# The image builds from the REPO ROOT: this module consumes gawk-server/wire
# through a local `replace`.
docker build -f gawk-telemetry/deploy/Dockerfile -t gawk-telemetry:dev ..
```

## Flags

Every flag has a `GAWK_TELEMETRY_*` environment fallback (flag > env > default).

| Flag | Env | Default |
|------|-----|---------|
| `-telemetry-key` | `GAWK_TELEMETRY_KEY` | (required — 64 hex chars, the relay's key) |
| `-ingest-addr` | `GAWK_TELEMETRY_INGEST_ADDR` | `:8080` (public) |
| `-read-addr` | `GAWK_TELEMETRY_READ_ADDR` | `:8081` (never public) |
| `-data-dir` | `GAWK_TELEMETRY_DATA_DIR` | `/data` |
| `-retention-days` | `GAWK_TELEMETRY_RETENTION_DAYS` | `14` (raw only; rollups are permanent) |
| `-scrape-interval` | `GAWK_TELEMETRY_SCRAPE_INTERVAL` | `5s` |
| `-session-idle` | `GAWK_TELEMETRY_SESSION_IDLE` | `2m` |
| `-relay-headless-service` | `GAWK_TELEMETRY_RELAY_HEADLESS` | (empty = client-only telemetry) |
| `-relay-metrics-port` | `GAWK_TELEMETRY_RELAY_PORT` | `2112` |
| `-relay-addrs` | `GAWK_TELEMETRY_RELAY_ADDRS` | (empty; overrides the headless Service) |
| `-dashboard-base` | `GAWK_TELEMETRY_DASHBOARD_BASE` | (empty) |
| `-mcp` | `GAWK_TELEMETRY_MCP` | `true` |
| `-query-sql` | `GAWK_TELEMETRY_QUERY_SQL` | `false` |
| `-stats-key` | `GAWK_TELEMETRY_STATS_KEY` | (empty = the find-a-stream lookup is off) |
| `-read-user` / `-read-password` | `GAWK_TELEMETRY_READ_USER` / `_PASSWORD` | (empty = no auth) |
| `-ingest-rate` / `-ingest-burst` | `GAWK_TELEMETRY_INGEST_RATE` / `_BURST` | `300` / `1200` (global) |
| `-ingest-session-rate` / `-ingest-session-burst` | `GAWK_TELEMETRY_INGEST_SESSION_RATE` / `_SESSION_BURST` | `1` / `10` (per session) |
| `-cors-origin` | `GAWK_TELEMETRY_CORS_ORIGIN` | (empty) |
| `-log-level` | `GAWK_TELEMETRY_LOG_LEVEL` | `info` |
| `-log-format` | `GAWK_TELEMETRY_LOG_FORMAT` | `json` |

The service **refuses to start without a key**: with none it could only reject
everything or accept anything, and neither is a mode worth having.
