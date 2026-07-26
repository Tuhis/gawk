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
| `-read-user` / `-read-password` | `GAWK_TELEMETRY_READ_USER` / `_PASSWORD` | (empty = no auth) |
| `-ingest-rate` / `-ingest-burst` | `GAWK_TELEMETRY_INGEST_RATE` / `_BURST` | `5` / `20` |

The service **refuses to start without a key**: with none it could only reject
everything or accept anything, and neither is a mode worth having.
