# R28 — Advanced diagnostics & telemetry (docs/33)

Design doc for [ROADMAP R28](../ROADMAP.md#r28--advanced-diagnostics--telemetry).

Every broadcast and every viewer session records what actually happened, into
a store that answers three questions with no human copy-pasting anything:
*how is this stream going right now*, *what was that specific viewer's
experience*, and *how does this compare to the average*. The headline consumer
is **Claude, not a dashboard** — "the stream stuttered last night, find out
why" should start from data the machine fetches and narrows itself.

---

## 1. Purpose

### 1.1 What R9 left on the table, and why the trigger fired

R9 named this exact feature and deferred it, with a condition attached
([docs/13](13-observability.md), Non-goals):

> **Client→server metrics push.** Browser clients can't be scraped, and
> shipping their stats to the relay (to re-export via Prometheus) is a new
> protocol surface + retention story for marginal gain at 15 viewers. The
> copy-diagnostics-JSON button covers the "get me that viewer's numbers"
> case. Deferred, not rejected — **revisit if remote troubleshooting of
> friends' sessions becomes routine.**

It became routine. R15 alone produced **twelve field findings**, every one of
them diagnosed from hand-shuttled Copy-diagnostics blobs; R19, R21 and R22 ran
the same loop. And the findings that took longest are precisely the ones a
single pasted 10-second window cannot support:

- **docs/20 finding 8** (the overflow/concealment latch) needed the *ratio* of
  two counters over time and a healthy session to compare against. It was
  identified only once someone thought to look at concealment-vs-overflow
  together — a comparison the store should make automatic.
- **docs/20 finding 12** (`avSkewMs` over-reports on long/stressed sessions)
  is *still open*, and it is open for a structural reason: it manifests on
  **long** sessions, and nobody has a long session's worth of samples. The
  R15 verification plan asks for "short healthy sessions" as a workaround.
- **docs/27 findings 1–5** were found across three on-device iPhone passes,
  each costing a full manual cycle, because the device is where the data was
  and the data did not survive the session.

R9's own cost/benefit still reads correctly — it was right *then*. What
changed is the denominator: the number of open questions that need history
rather than a snapshot.

### 1.2 What already exists (this doc adds no measurement layer)

| Surface | Where | Shape |
|---------|-------|-------|
| `ViewerStats` | `gawk-app/src/transport/viewer.ts` | ~80 fields, 500 ms tick, incl. R5 latency/drift, R12 jitter, R15 audio + `audioBuffer`, R18 count, R19/R21 delivery, R22 `presentationMux` |
| `BroadcastStats` | `gawk-app/src/transport/broadcaster.ts` | funnel (capture→gate→encode→sent), encoder queue/latency, R4 ladder, R13 ceilings |
| `engine.Stats` | `gawk-broadcast/internal/engine` | the native broadcaster's equivalent, incl. R18 `ViewerCount` |
| `RegistryStats` / `SubscriberStats` | `gawk-server/internal/hub/hub.go:312` | per-broadcast + **per-subscriber** detail, incl. R9 ingress loss, carrier + DVR counters |
| `DiagnosticsBuffer` | `gawk-app/src/lib/diagnostics.ts` | a bounded rolling window that already serializes exactly the JSON blob this design wants to *send* |
| Prometheus | `gawk-server/internal/metrics`, `internal/ops` | fleet-health families on a plain-TCP ops port |

R28 adds a **pipe, a store, and a query surface**. If a chunk finds itself
adding a new measurement, that is a signal it drifted.

### 1.3 The load-bearing gap

`/statusz` reports `subscriberDetails[].key` — a random per-session handle
(`newSubscriberStatsKey`, `hub.go:2845`) — and **nothing ever tells the client
its own key**. So the relay's view of a viewer and that viewer's view of
itself are two datasets that cannot be joined. "Per-viewer experience" is
exactly that join. Everything in this design is downstream of an identifier
that both ends know (D2).

---

## 2. Decisions locked with the owner (2026-07-26)

| Axis | Decision |
|------|----------|
| Architecture | A new **optional `gawk-telemetry` service**, **files-first**: gzipped NDJSON session artifacts on a PVC, queried by plain Go (DuckDB for ad-hoc — D11). Chart-gated, default off; a deployment that does not enable it behaves exactly as today |
| Retention | **14 days full fidelity**, then pruned by partition-directory delete — plus a **permanent per-session rollup** row |
| Read surfaces | **All four**: an **MCP server with `diagnose()`** (primary, machine-facing), a **plain HTTP JSON API**, a **minimal built-in web dashboard**, and the **Grafana dashboard deferred since R9 M8** |
| Collection | **Always on for every viewer and broadcaster, zero PII** — no IP addresses stored, coarse browser/OS class instead of the UA string, no cross-broadcast identity, no fingerprinting. R23's terms text updated to say so plainly |

---

## 3. Design decisions

### D1 — Collection is out-of-band HTTPS, not in-band WebTransport

Telemetry must **outlive the transport it is reporting on**. The failures
worth catching — the Safari stream wedge, a 4002 rollout blip, the R22
underrun pause, "it just froze" — are transport or session failures, and stats
that ride the failing session die exactly when they become interesting. That
alone settles it, and two further properties fall out:

- `navigator.sendBeacon` on `visibilitychange → hidden` delivers the final
  batch after the page is gone. An in-band path has no such hook: the session
  is already closing.
- Retry with backoff is possible, because the telemetry path has its own
  independent failure domain.

**The endpoint is a same-origin path on the frontend host** — the gawk-app
Ingress (`nginx-int`, cert-manager TLS, `gawk-app/deploy/charts/gawk-app/values.yaml`)
routes `/api/telemetry/` to the `gawk-telemetry` Service. No new
LoadBalancer, no new DNS record, no new certificate. Same-origin also removes
CORS from the picture entirely, which matters more than it sounds: a
cross-origin `sendBeacon` with `Content-Type: application/json` needs a
preflight it cannot perform during unload, and the usual workaround is to lie
about the content type. Same-origin needs neither.

The default ingest URL is therefore **the same-origin path, with no
configuration at all**; `config.js` may override it (an operator splitting the
services), and that override is the only case that has to think about CORS.

**Only the ingest path is routed publicly.** The read API and the dashboard
sit on a separate listener that the public Ingress never exposes (D14) — the
write surface has to be reachable by every viewer, the read surface has no
reason to be reachable by any of them.

### D2 — The correlation ID: a relay-minted, stateless-verifiable session token (wire `0x0D`)

The relay mints a per-session token and delivers it **in-band** after upgrade
— the browser WebTransport API exposes no response headers, the same
constraint that put R17's resume token (0x09) and R21's DeliveryAck (0x0C) on
the wire. New message **`TypeTelemetryHello = 0x0D`** (next free; 0x0C is
R21's), relay→client, sent once per session on both `/publish*` and
`/subscribe/{id}`; clients parse it and never send it.

It carries three things the client cannot otherwise know:

1. **The session token** — the ingest credential *and* the join key.
2. **The obfuscated broadcast key** — so the client never sends the raw
   broadcast ID (D8).
3. **Whether telemetry is enabled at all, and at what cadence** — the relay
   is the authority, so a disabled deployment produces clients that never
   collect anything rather than clients that POST into a void.

The token is a **stateless HMAC**, exactly R17's resume-token pattern
(`gawk-server/wire`, keyed by a shared fleet Secret): any relay pod mints it,
`gawk-telemetry` verifies it with no lookup, no shared database, and no
chatter with the relay. That buys the property that makes an unauthenticated
public write surface tolerable: **the ingest only accepts records from clients
that actually connected to a relay in this fleet**.

Relation to the existing `statsKey`: they stay **independent**, and the
relay's own scraped record carries both. `statsKey` remains the 32-bit
`/statusz` display handle it is today (unchanged semantics, unchanged tests);
`subscriberDetails` gains an additive `sessionId` field, and the relay is the
only party that sees both — which is where the join happens. Deriving one from
the other would leak part of a bearer credential into a public-ish JSON
endpoint for no benefit.

### D3 — Storage: hive-partitioned NDJSON, 14 days raw + permanent rollups

```
/data/
  sessions/date=2026-07-26/broadcast=<12 hex>/<sessionId>.ndjson[.gz]
  rollups/date=2026-07-26.ndjson          # permanent, one line per session
  relay/date=2026-07-26/<pod>.ndjson[.gz] # scraped relay snapshots (D5)
```

`date=`/`broadcast=` are hive-style partitions, so both plain Go and a DuckDB
`read_json_auto(..., hive_partitioning=1)` prune by path instead of scanning.
Retention is then **a directory delete**, not a query.

A session file is written **plain while the session is open** and gzipped on
finalize (session end, or an idle timeout for sessions that vanish). Appending
to a gzip stream is legal but awkward to reason about under crash; a startup
sweep that finalizes orphaned `.ndjson` files is a directory scan and obviously
correct. One file per session means one writer per file — no interleaving, no
locking.

### D4 — The rollup schema is the one genuinely costly decision

Raw samples are disposable — 14 days and gone, and their shape may drift with
every milestone. The **rollup row is permanent**, so it is the only place
where getting the schema wrong is expensive. Rules:

- **Additive forever.** Fields are appended, never renamed or repurposed. A
  reader must tolerate rows from older releases missing newer fields — which
  sparse JSON gives for free, and which the query layer must not undermine by
  assuming presence.
- **Percentiles, not means, for anything experiential.** A mean fps over a
  session with one 4-second freeze looks fine. Median + p95 (+ p05 where the
  bad tail is low, e.g. fps) is the shape.
- **Store the verdict, not just the numbers.** `diagnose()`'s output at
  session end is recorded on the row (D6), which is what makes "has this got
  better since the R15 fix?" a single query over rollups instead of a
  re-analysis of raw windows that no longer exist.

Full field list in §4.6.

### D5 — The relay is *scraped*, and never pushes

`gawk-telemetry` polls each relay pod's existing ops `/statusz` (R9 M1) and
derives relay-side session records itself. The relay gains **no outbound HTTP
client, no telemetry queue, and no new failure mode**. That is not fastidiousness:
the relay process carries the media hot path for every broadcast on the pod,
and R19's finding 12 and R21's design are both about how a queue in that
process becomes a stall in someone's video. The one thing this design must
never do is put a telemetry backpressure path inside it.

Consequences, accepted and recorded:

- **Sessions shorter than the scrape interval are invisible to the relay
  side.** Their rollups are client-only and carry
  `relayCoverage: "none" | "partial" | "full"`, so a verdict never silently
  rests on a relay view that does not exist.
- **Close-time folded totals are missed** — a subscriber's final counters are
  folded into hub totals on close. The last pre-close scrape is the record;
  the delta to the fold is bounded by the scrape interval. Default 5 s.
- **Per-pod scraping needs per-pod addresses.** The existing metrics Service
  is a ClusterIP and load-balances, so scraping it hits one random pod. The
  relay chart gains an **optional headless companion Service**
  (`clusterIP: None`) whose DNS A records enumerate the pods; telemetry
  resolves it each interval. The existing metrics Service and its
  ServiceMonitor are untouched — Prometheus's scrape path must not change
  shape because telemetry showed up.

### D6 — `diagnose()`: the docs/13 playbook as code

The [bottleneck playbook](13-observability.md#bottleneck-playbook) is a
14-row symptom → discriminating-signals → verdict table. It is already written
and already correct; it has been the actual debugging procedure since R9.
`diagnose()` executes it.

Each rule is a small declarative record: `id`, the signals it *requires*,
a predicate over a session (or a broadcast's sessions) in a time window, a
verdict, and the **evidence** — the concrete numbers the predicate fired on.
Output is a **ranked list of candidates with evidence and confidence**, plus
an explicit list of signals that were unavailable, never a single asserted
answer. Two additions telemetry makes possible that the human procedure never
had:

- **Self-comparison**: this session's bad window against its own healthy
  window.
- **Fleet comparison**: this session against the rollup median for the same
  delivery mode / browser class / rung.

This is the chunk that decides whether R28 succeeded. An MCP surface that
returns raw samples is *worse* than today's copy-paste, because it spends
context to arrive at the same place.

### D7 — Relay numbers anchor; client numbers are testimony

A verdict must never let a client's self-report override a relay counter that
contradicts it. This is a correctness stance, not a security one — the trust
model is a known operator (CLAUDE.md), and nobody is forging telemetry. The
point is subtler and was earned the hard way in docs/20 finding 7: **a wedged
client's own accounting is the least reliable evidence in the system, and it
is exactly what a wedged client sends.** `queuedMs` was a shadow of the
worklet's real depth; it was confidently wrong; it drove the drop decisions.

So each rule declares its evidence's provenance (`relay` | `client` |
`derived`), and a rule resting only on client evidence caps its own
confidence. Where the two disagree, **the disagreement is itself a finding**
and gets surfaced rather than resolved silently.

### D8 — Zero-PII envelope, and where PII would otherwise leak in

"No PII" is a claim that has to survive the implementation, so name every
place it could leak:

| Leak | Mitigation |
|------|-----------|
| Raw broadcast ID (joinable, only ~31^6 strong — R9 D3) | The client never learns an ID it should not send: `TelemetryHello` carries the **obfuscated** key (`ObfuscateID`, `hub.go:1322`), and the ingest rejects anything else in that field |
| Full `navigator.userAgent` | Reduced **client-side** to browser family + major version + OS family (`"Chrome 152"` / `"Windows"`). The raw string never leaves the device |
| Source IP in the service, its logs, or the ingress access log | The service never reads, stores or logs the source IP; the chart's nginx annotations disable access logging on the telemetry path. Rate limiting uses the IP **without persisting it** — the same posture as R2's limiter |
| Fingerprintable extras (screen geometry, device memory, exact codec support matrices) | Not collected. The stats objects are pipeline metrics; anything identifying is an addition, not a subtraction, and must be argued for |
| Broadcast/session correlation across time | Session tokens are per-session and per-broadcast; there is no viewer identity, no cookie, no persisted client ID |

R23's terms are updated (TM2): the existing text already reserves broad
monitor/analyze rights, and its factual "as currently operated, no persistent
media recording" **stays true** — telemetry is metrics, never media. But
silence about an always-on collection is not good enough; the practice
statement gains a sentence naming what is collected and what is not, and
`termsVersion` bumps.

### D9 — Never on the media hot path; an ingest failure is invisible

Collection is idle-time work on already-computed objects, under a hard byte
budget. Ingest is fire-and-forget: a failed POST is retried with backoff up to
a small cap and then **dropped**, silently, forever. No user-visible error, no
retry storm, no unbounded buffer. If telemetry can degrade a stream, the item
has failed on its own terms — so this is an acceptance criterion (TM2), not an
aspiration.

### D10 — Context discipline is an API property, not a habit

The failure mode this item exists to prevent is reachable by building it
carelessly: 80 fields × 200 samples of JSON is context incineration. So the
read API enforces it structurally:

- Every tool has a **bounded default response**. `get_session` returns a
  downsampled projection (default ~40 points, a curated field set); the full
  firehose requires naming fields *and* a window explicitly.
- `diagnose()` returns verdicts + evidence, never the underlying series.
- A documented **soft ceiling of 32 KB** on any default tool response, and a
  test that asserts it against a synthetic 4-hour session.

### D11 — DuckDB is a query option, not a runtime dependency

*Refines the ROADMAP sketch, which said "DuckDB rollups".* The arithmetic
does not support making it a dependency: 14 days of rollups is a few thousand
JSON lines, and a first-class endpoint reads either one session file or the
rollup set. Plain Go does that in milliseconds.

So: **all first-class endpoints are plain Go** over rollups + session files —
which keeps `gawk-telemetry` cgo-free, dependency-light and buildable
everywhere. DuckDB stays exactly where it earns its keep: **ad-hoc
exploration** over the raw window, as a documented recipe (`read_json_auto`
with hive partitioning, straight off the PVC), plus an *optional*,
default-disabled `query_sql` MCP tool for when a question has no endpoint yet.
The storage layout (D3) is chosen to make that recipe work well; nothing in
the service depends on it.

### D12 — A fourth module and chart, default off

`gawk-telemetry/` becomes a top-level Go module beside `gawk-broadcast/`,
following its established pattern exactly: `replace github.com/Tuhis/gawk/gawk-server => ../gawk-server`
for the shared token/wire code (R14 Decision 1's precedent — import `wire`,
never mirror it), its own release-please component, its own image and Helm
chart. It is **optional**: `telemetry.enabled: false` by default in both the
relay chart (mint no tokens, expose no headless Service) and the app chart
(no ingest route). An install that never enables it is byte-identical to
today, and that is checkable by `helm template` diff (TM3).

### D13 — Client collection lives on the main thread; zero worker changes

Both stats objects already arrive on the main thread fully assembled — the
viewer screen merges in `audioBuffer`, `featureGates` and
`presentationSurface` before stats reach the overlay, and `BroadcastStats`
comes back through the worker shell the same way. The collector therefore
subscribes to the same stats the overlay renders. **No worker message, no
transport change, no pipeline change**, on either surface. The native
broadcaster (R14/R25) gets a small reporter goroutine over `engine.Stats`.

### D14 — The built-in dashboard is a first-class surface, and Grafana answers a different question

Both were asked for; building the same view twice would waste that. The split
is by question, and by *time horizon*:

- **Built-in dashboard** (served by `gawk-telemetry`, Go `embed`, no build
  step): **what is happening right now** — every live broadcast, its
  broadcaster, and every one of its viewers, with anything obviously wrong
  **highlighted without the operator going looking for it**. Plus the
  after-the-fact session page with sparklines and the `diagnose()` verdict.
  This is the surface that replaces "ask the friend to paste a blob", and it
  is the only surface a human uses *while a stream is live*. Full design in
  §4.8.
- **Grafana** (finally closing R9 M8): **trends over time** — fleet health,
  release-over-release comparison, capacity — driven by Prometheus as R9
  designed it. Rollup-derived panels are a stretch that would need a
  JSON/Infinity datasource pointed at the read API; flagged as an open
  question (§8), not a commitment.

The consequence, recorded because it reverses an earlier ordering in this
doc's own drafting: **the dashboard is not the droppable half.** A live
operational view is the thing an owner reaches for at 21:00 when a friend says
"it's stuttering", and it is what makes the always-on collection worth having
before any AI is involved. Grafana (TM9) is the part that can slip; the
dashboard (TM8) cannot.

**Exposure**: only the *ingest* path is public (D1). The dashboard and the
read API live on a **separate listener**, exposed via ClusterIP and, at the
operator's discretion, an internal-only Ingress — never on the public path
that carries ingest. That is R9 D1's posture for `/metrics`, for the same
reason: the read side aggregates every broadcast on the fleet, and it should
be no more reachable than `/statusz` is today. Consistent with that
precedent, it carries no auth by default (cluster-internal), with optional
basic auth for operators who route it through an Ingress.

### D15 — Strict envelope, tolerant payload, typed rollup

Validation is not one policy. A batch carries two kinds of data with opposite
requirements, and applying one stance to both breaks something either way.

**The envelope is protocol: strict, reject on violation.** `v`, `token`,
`role`, `broadcastKey`, `seq`, `final`, `app.*`, `startedAtMs`, and the
*structure* of `samples`/`events` (array; each entry a finite numeric `tMs`
and an object `stats`). Wrong type, missing field or unknown `v` ⇒ **400 and
nothing written**. It is small, stable, security-relevant, and it is the same
posture `wire` takes ("strict, like every other parser here").

**The stats payload is data: tolerant reader, never reject.** Strictness here
would actively hurt, for a structural reason rather than a stylistic one —
**version skew is permanent, not transient**. Stats objects are produced by a
browser SPA a viewer may have loaded hours ago, and `ViewerStats` has grown in
R5, R9, R10, R12, R15, R16, R18, R19, R21 and R22. A closed field list on the
service means shipping a gawk-app with a new field rejects every batch from
updated clients until the service is redeployed, and an old open tab is
rejected forever — losing telemetry exactly during a deploy, which is when it
is most wanted. R17 already wrote the rule down for internal protocols: skew
tolerance is mandatory. So:

- **Known fields are typed.** A declared field list with expected types; a
  wrongly-typed value is **dropped and counted**, never fatal. This matters
  most for anything feeding percentile math or a `diagnose()` predicate — a
  string `"30"`, a `null`, or a non-finite number must never silently become
  a data point in an fps series.
- **Unknown fields are kept verbatim** into the NDJSON. A new client's new
  field survives an old service and becomes queryable the day the service
  learns its name. The store is schemaless, so this costs nothing.
- **`app.version` in the envelope is the schema version.** No separate field:
  every sample is already attributable to the exact release that produced it.

**Structural bounds are enforced regardless of types** — max samples per
batch, max events, max fields per stats object, max nesting depth, max string
length. This is the part that actually protects the disk and the JSON parser,
and being type-agnostic it does not rot as the stats objects grow.

**Schema quality is a signal, not an error.** Coerced, dropped and unknown
field counts are tallied per session and land on the rollup row
(`schemaAnomalies`), so "this client is sending nonsense" becomes a
diagnosable fact rather than a silent hole. Both directions are useful: a
spike of *unknown* fields means clients are running ahead of the service, a
spike of *coercions* means a real client bug.

**The rollup is strictly typed, by construction.** It is the permanent
artifact (D4) and therefore the one worth rigidity — and because the service
computes it from already-validated inputs, a value that could not be computed
is **absent**, never a wrongly-typed guess. Raw samples are disposable in 14
days; put the rigor where the permanence is.

---

## 4. Mechanism

### 4.1 Wire: `TelemetryHello` (0x0D)

Fixed 35 bytes, big-endian, strict-parsed like every sibling message, golden
vectors mirrored into all three `wire` implementations (Go, `wire.ts`,
`gawk-broadcast`'s `wirecheck`):

| Offset | Size | Field |
|--------|------|-------|
| 0 | 1 | `Version` (0x01) |
| 1 | 1 | `TypeTelemetryHello` (0x0D) |
| 2 | 1 | flags — bit 0 `enabled`; bits 1–7 reserved, must be 0 |
| 3 | 2 | `reportIntervalMs` (uint16) |
| 5 | 24 | session token (§4.2) |
| 29 | 6 | obfuscated broadcast key (raw digest; hex-encoded by the client) |

Sent once per session, after upgrade, on `/publish`, `/publish/{id}` and
`/subscribe/{id}`. **Not** sent on `/internal/subscribe` — an edge is
plumbing, not a client (R17 W4's `internal` flag already marks it). Clients
parse and never send; a `TelemetryHello` arriving where a client is the sender
is dropped, matching `TypeViewerCount`'s rule.

`enabled: 0` means the fleet has telemetry off: the client collects nothing
and the remaining fields are ignored. A relay predating R28 sends nothing at
all, which the client treats identically — so an old relay and a
telemetry-disabled relay produce the same (correct) client behaviour.

### 4.2 The session token

```
token = expHour (uint32 BE) ‖ nonce (12 B) ‖ tag (8 B)          # 24 bytes
tag   = HMAC-SHA256(telemetryKey, expHour ‖ nonce ‖ broadcastKey ‖ role)[:8]
```

- `telemetryKey` is a 32-byte fleet Secret (chart-provisioned, same shape as
  R17's `statsKey`/`internalPsk`/`resumeTokenKey`). Absent ⇒ telemetry
  disabled, `enabled: 0`.
- `expHour` is mint time + 24 h in unix hours. The verifier rejects expired
  tokens with one HMAC and no bucket sweep — a long broadcast is well inside
  the window, and a token that outlives its session is only useful for
  submitting records that would be attributed to a session that ended.
- `role` ∈ {`viewer`, `broadcaster`} is bound into the tag, so a viewer's
  token cannot submit broadcaster-shaped records.
- **`sessionId` = hex(nonce)** (24 chars). It is what appears in storage,
  filenames, `/statusz` `subscriberDetails[].sessionId`, and every read API.
  The *token* — the credential — is never stored, by either the relay
  (stateless) or the service (verify-and-discard).

A forged token requires the fleet key. A replayed one is bounded by expiry
and, being tied to one `sessionId`, can only pollute a session that already
exists.

### 4.3 Ingest envelope, batching, budget

`POST /api/telemetry/v1/ingest`, `Content-Type: application/json`,
optionally `Content-Encoding: gzip`:

```jsonc
{
  "v": 1,
  "token": "<48 hex>",
  "role": "viewer",
  "broadcastKey": "<12 hex>",
  "seq": 3,                    // batch counter; gaps are recorded, not fatal
  "final": false,              // true on the unload beacon
  "app": { "version": "0.9.1", "surface": "viewer",
           "browser": "Chrome 152", "os": "Windows" },
  "startedAtMs": 1769424000000,
  "samples": [ { "tMs": 0, "stats": { /* ViewerStats verbatim */ } } ],
  "events":  [ { "tMs": 8123, "kind": "reconnect", "detail": "close 4002" } ]
}
```

**Events matter as much as samples.** A 2-second sample grid cannot represent
a reconnect, a close code, a delivery-mode switch, an R4 ladder step, a
decoder error, or an R22 presentation handoff — and those are exactly what a
human narrates when describing what went wrong. They are cheap and they carry
the story.

Sizing, which is what justifies files-first over any real database:

- A `ViewerStats` sample is ~1.5 KB of JSON. Reporting cadence is ~2 s (the
  overlay keeps its 500 ms tick), so ~750 B/s/viewer uncompressed.
- Batches flush every ~10 s ⇒ ~7.5 KB, comfortably under the browser's 64 KB
  `keepalive`/`sendBeacon` body cap. `CompressionStream('gzip')` where
  available (feature-detected, like everything else here) takes it to ~1 KB —
  NDJSON of near-identical records compresses ~10×.
- Ten viewers × 2 h/day ≈ **5 MB/day stored**, ≈ **75 MB for the whole 14-day
  window**. A PVC and a prune job cover it; ClickHouse would be answering a
  question nobody asked.
- Hard per-session byte budget (default 4 MB ≈ a 5-hour session). On
  exhaustion the collector drops to events-only and records that it did — a
  truncated session must never look like a complete one.

Validation of this envelope is strict; validation of the `stats` objects
inside it deliberately is not, and the structural bounds that hold either way
(samples per batch, fields per object, nesting depth, string length) are in
**D15**.

Flush hooks: the periodic timer, and `visibilitychange → hidden` via
`sendBeacon` (`pagehide`/`unload` are unreliable on mobile; `hidden` is the
one that fires, and it is bfcache-compatible).

### 4.4 Relay scrape and join

Every `scrapeInterval` (default 5 s), for each pod resolved from the headless
Service: `GET /statusz` → append one line per broadcast and per subscriber to
`relay/date=…/<pod>.ndjson`, stamped with the observation time and the pod's
role (origin/edge, already in `/statusz` since R17).

The join is `sessionId`, present on both sides: the client's own records under
`sessions/`, the relay's observations under `relay/`. At finalize the rollup
row carries both, plus `relayCoverage` (D5).

### 4.5 Rollup row (permanent — §D4's rules apply)

One line per session in `rollups/date=….ndjson`:

- **Identity**: `sessionId`, `broadcastKey`, `role`, `relayPod`, `relayRole`,
  `appVersion`, `relayVersion`, `browser`, `os`, `startedAt`, `endedAt`,
  `durationMs`, `relayCoverage`.
- **Data quality** (D15): `schemaAnomalies` — counts of coerced, dropped and
  unknown stats fields, plus batch `seq` gaps and whether the session hit its
  byte budget. A verdict computed over a session with a high anomaly count is
  a verdict to distrust, and this is what makes that visible.
- **Configuration** (what this session *was*): delivery mode, playout mode,
  interpolation on/off, resolution + fps rung, codec, hardware acceleration,
  `pipelineContext`, `renderer`, `transport`, `presentationSurface`.
- **Funnel** (median/p05 or p95 as appropriate): viewer `receivedFps`,
  `decoderFps`, `renderedFps`; broadcaster `captureFps`, `encoderFps`,
  `sentFps`.
- **Health**: stall count, total and longest stall ms, `reorderGapResyncs`,
  `framesDroppedIncomplete`, `framesDroppedLate`, keyframe waits,
  `configsApplied`, reconnect count + close codes seen.
- **Latency/jitter**: median + p95 of `capToRenderMs`, `liveEdgeDriftMs`,
  `timeSyncRttMs`, `playoutOffsetMs`, `arrivalJitterMs`,
  `renderCadenceP95Ms`.
- **Audio**: `audioState`, `avSkewMs` median/p95, underruns, `gapsConcealed`,
  `gapsSkipped`, `overflowDrops`, `resets`, `contextSampleRate`.
- **Relay side** (joined): `dropped`, `sendErrors`, `keyframesSent/Dropped`,
  carrier counters, DVR counters, and the broadcast's `ingressFramesLost`.
- **Verdict**: `diagnose()`'s ranked output at session end, with evidence and
  confidence.

### 4.6 Read API and MCP tools

One implementation, two façades — the MCP server is a thin wrapper over the
same handlers, so they cannot drift:

| Tool / endpoint | Returns | Default bound |
|---|---|---|
| `list_broadcasts(since, limit)` | broadcast summaries with session counts + worst verdict | 50 rows |
| `list_sessions(broadcast?, since, role?, verdict?, limit)` | rollup rows, projected | 50 rows |
| `get_session(sessionId, fields?, window?)` | downsampled timeline + events | ~40 points, curated fields |
| `diagnose(sessionId \| broadcastKey, window?)` | ranked verdicts + evidence + unavailable signals | verdicts only |
| `compare(sessionIds[] \| session vs fleet)` | field-by-field deltas vs the fleet median for the same class | 1 table |
| `fleet_summary(since, groupBy?)` | the "overall average": percentiles by delivery mode / browser / rung | 1 table |
| `query_sql(sql)` | DuckDB passthrough — **optional, default disabled** (D11) | operator-set |

### 4.7 Rule shape for `diagnose()`

```
Rule {
  id            "leg-b-single-viewer"
  requires      [relay.subscriber.dropped, relay.broadcast.ingressFramesLost,
                 client.timeSinceLastFrameMs]
  predicate     this subscriber's drop rate ≫ peer median AND ingress loss ≈ 0
  verdict       "Leg B for this viewer — their downlink or machine"
  evidence      the actual numbers, each tagged relay | client | derived
  confidence    capped when only client-provenance evidence fired (D7)
}
```

The initial rule set is the docs/13 playbook's 14 rows, transcribed. New rules
are how a field finding gets institutionalized: docs/20 finding 8's
concealment-vs-overflow ratio is a rule; finding 12's `avSkewMs` over-report
becomes a rule the moment its signature is known — and the 14-day window is
what will let it be known.

### 4.8 The live dashboard

The operator-facing half of the item (D14). Its single design goal:
**someone who opens it during a live stream should see whether anything is
wrong before they click anything.** Everything below follows from that.

#### 4.8.1 Three views, deep-linkable

1. **Fleet view** (the landing page) — two groups, never interleaved.
   **Live** first: one row per live broadcast with short broadcast key,
   uptime, broadcaster health, viewer count, **worst viewer health**, and the
   two or three relay signals that matter at a glance (ingress loss, egress
   drops, publisher present/away), sorted by **severity, not recency** — so
   problems float to the top and a healthy fleet is a short quiet list. Then
   **Recently ended** below it (owner decision 2026-07-26): the last ~10
   broadcasts within ~6 h, recency-sorted, each carrying its stored
   `diagnose()` verdict. That group is served straight from the rollup index
   (permanent, tiny), not the live projection — so it costs nothing and it
   shows the *final* verdict rather than a re-computation.
   **A live `warn` always outranks an ended `bad`**: the grouping is the
   precedence, because only the live one can still be acted on. An empty
   fleet is then honest *and* useful — you land on what just happened instead
   of a blank page.
2. **Broadcast detail** — the broadcaster and its viewers **in one table**,
   because they are the same kind of thing: both are sessions with tokens,
   distinguished by a `role` column, with the broadcaster pinned first. The
   broadcaster row carries the send-side funnel (capture → gate → encode →
   sent), encoder queue and pressure, active rung/codec, and uplink signals;
   each viewer row carries delivery mode, received/decoded/rendered fps,
   stall state, live-edge drift and capture→render latency, audio state, and
   its own health. Any row expands to a sparkline strip over the last few
   minutes.
3. **Session detail** — the after-the-fact view of one ended session: the
   full timeline, the events (reconnects, close codes, ladder steps, mode
   changes), and the stored `diagnose()` verdict with its evidence.

Every view is **deep-linkable** (hash routes, matching the main app's
convention), so "look at this" is a pasted URL. `diagnose()` output includes
the dashboard URL for the session it analysed — which is what lets Claude hand
a human something to *look* at rather than a wall of numbers.

#### 4.8.2 Where "live" comes from

The live view is served from an **in-memory projection**, not from disk: the
relay scraper (D5) refreshes the relay side every ~5 s, and ingest refreshes
the client side as batches land (~10 s). Disk is for history; the live page
never reads a session file.

That means the two halves of a row have **different freshness**, and the UI
says so rather than painting them as one instant: relay-side values are ≤ 5 s
old, client-side values ≤ ~15 s. A viewer whose client telemetry has gone
quiet while the relay still sees the subscriber is shown as **stale**, and a
subscriber that never reports at all (an old client, a blocked endpoint) is
shown as **unknown** — never as healthy. Painting an absence of evidence as
green is the one thing an ops dashboard must not do.

The page **polls one endpoint** (`/live`) every 2 s. Not SSE or WebSocket:
polling has no connection state to lose, survives any proxy, is trivially
debuggable with `curl`, and 2 s against an in-memory projection costs
nothing at this scale.

#### 4.8.3 Health model — one rule engine, two windows

Severity comes from **the same rules as `diagnose()`** (D6), evaluated over
the live rolling window instead of a stored session. Two truths about the same
stream disagreeing on a dashboard would be worse than no dashboard, so there
is one engine; live evaluation simply uses the subset of rules whose signals
are available continuously.

Four states, deliberately few: **ok · warn · bad · unknown**. Lifecycle
(`live` / `away` / `ended`) is carried *separately* — a broadcaster in the
R1 grace period is `away`, which is not a fault.

The two dimensions are orthogonal and mean different things, which the
presentation has to respect (§4.8.4). A **live** row's severity is a claim
about *right now*, computed over the rolling window; an **ended** row's is a
claim about *how the whole session went*, read from the stored verdict. So
they are labelled differently — `LIVE · warn` against `ended 14 min ago ·
2 issues` — because "is stuttering" and "stuttered" are not the same
statement, and a dashboard that renders them identically invites acting on a
problem that is already over.

What lights a row up, by side:

| Side | Warn / bad signals |
|---|---|
| Broadcast | relay ingress loss above threshold; publisher active but no frames relayed; egress drops with `reason="bandwidth"`; keyframe drops with `reason="slow"` across *all* subscribers; approaching a configured limit |
| Broadcaster session | encode funnel gap (post-gate fps ≫ encoder fps), encoder queue growing, sent fps below encoded fps, repeated reconnects |
| Viewer session | stalled (`timeSinceLastFrameMs` beyond a GOP multiple); received fps far below the broadcaster's sent fps; decoded ≪ received; that subscriber's queue/carrier drops or DVR resyncs climbing; playout offset pinned at its clamp; audio underruns/overflow drops; reconnect churn |
| Either | no telemetry despite the relay seeing the session (**unknown**, never ok) |

**Escalation is hysteretic**, following the project's dwell instinct (R4, R27):
a state must hold for two consecutive evaluations to escalate, and clears only
after a dwell (~15 s). For an ops view the asymmetry is deliberate — problems
should appear promptly and must not vanish before the human finishes looking
at them.

#### 4.8.4 Presentation

Plain HTML + a small vanilla JS file, embedded in the binary (Go `embed`),
**no build step and no external asset fetch** — the dashboard must work on a
port-forward from a laptop with no network.

It is deliberately **not** part of R6's design system: different origin,
different build, no shared tokens, and coupling an ops page to the product's
component library buys nothing. It does borrow R6's **monochrome restraint**,
for a functional reason — in an otherwise monochrome table, one amber row is
unmissable.

Colour therefore carries **two orthogonal channels, and nothing else**:

- **Severity → hue.** Amber and red accents, reserved exclusively for
  `warn`/`bad`; `ok` and `unknown` spend no hue at all.
- **Lifecycle → contrast.** Live rows at full contrast, ended rows
  **recessed** (desaturated, lower contrast), `away` between them. So an
  ended `bad` still shows its red — you can see at a glance that last
  night went badly — but it cannot out-shout a live `warn` sitting above
  it. Recession is what lets the ended group be present without competing
  for attention, which is the whole reason it is safe to show it.

**Neither channel is ever encoded by colour alone.** Every row carries a
status glyph and a text label (`LIVE`, `away`, `ended 14 min ago`) alongside
the severity word, so the page survives a colour-blind reader, a greyscale
screenshot pasted into a chat, and the ended group being distinguishable at
all if the CSS fails to load.

No login, no time-range picker on the live view (it is live), no
configuration. The fleet view is the whole product for the common case.

---

## 4.9 Implementation status & deviations (2026-07-26)

**TM1–TM8 implemented; TM9 (Grafana) dropped by owner decision.** Automated
gates green in all four modules (`gawk-server` gofmt/vet/`-race`,
`gawk-telemetry` gofmt/vet/`-race`, `gawk-broadcast` vet/tests,
`gawk-app` tsc/vitest/oxlint/build) plus `helm lint`/`helm template` on all
three charts. **Manual verification (§6) is pending** — none of the eight
numbered passes has been run.

Deviations from the design as written, recorded rather than silently absorbed:

1. **The hello rides a reliable uni stream, not a datagram** (owner decision).
   §4.1 said "sent once per session, after upgrade" without naming a carrier,
   and the two precedents disagree: ResumeToken (0x09) uses a uni stream,
   DeliveryAck (0x0C) used a datagram and had to grow a re-announce loop
   because one-shot join-time datagrams get lost. A lost hello is worse than a
   mislabelled row — it is a session that silently never reports — so the
   ResumeToken precedent won. The viewer already dispatches server uni streams
   by wire type, so 0x0D slots in beside StreamFrame and ReliableCarrier.

2. **D13's "no worker message" holds for collection, not for the hello.**
   Collection genuinely lives on the main thread and adds no worker traffic.
   But wire 0x0D *arrives* inside the viewer's nested transport worker, so it
   crosses as its own message on both worker hops (`telemetryHello`).
   Deliberately its own message rather than a `ViewerStats` field: the token is
   a bearer credential, and `ViewerStats` is what Copy diagnostics serializes
   for pasting into a chat.

3. **D8's framing was loose; the mechanism is what was implemented.** A viewer
   obviously *does* know the raw broadcast ID — it is in `#/view/<id>`. What
   prevents it being reported is that the session token's HMAC binds the
   *obfuscated* key, so a batch carrying anything else fails verification. The
   ingest rejects it as a 400 before a byte is written.

4. **A payload number can never reject a batch.** Go's JSON decoder *errors* on
   a number overflowing float64, so decoding `stats` inline would have let one
   absurd value inside a payload reject the whole batch — exactly the
   strictness D15 rules out for payload data. `stats` is therefore kept raw at
   envelope-decode time and decoded separately with `UseNumber`, turning the
   overflow into a drop-and-count. Found by the test written for D15's
   tolerance criterion.

5. **Truncation is announced, not just flagged.** Setting the byte-budget flag
   was not enough: once samples stop, every later flush is empty and returns
   early, so the marker would never have reached the wire and a clipped session
   would have read as one that simply ended. Crossing the budget now emits a
   `telemetry-budget-exhausted` event, which guarantees at least one more
   batch carrying `truncated`.

6. **p05 does not catch a short freeze, and the rollup says so.** The
   acceptance criterion's "mean hides a freeze" argument stops working at
   percentiles too: two bad samples in 122 is 1.6 %, below the 5th percentile,
   so `p05` correctly reads 30 fps through a 4-second stall. That is precisely
   why the row carries explicit `stalls`/`longestStallMs`/`totalStallMs`
   fields, counted as *episodes* rather than samples. The test asserts both
   halves, including the limit.

7. **Freshness overrides severity *after* hysteresis, not before.**
   Hysteresis is for rule-derived severity, which is a judgement over noisy
   measurements. "This client has been silent for 30 s" is a clock reading with
   no blip to filter; delaying it would have shown a viewer as healthy after it
   demonstrably stopped reporting — the exact failure the override exists to
   prevent.

8. **Stored sessions are self-describing on read.** `ParseTimeline` originally
   took the role from the live state only, so a session read back from disk
   lost it and every role-scoped rule was silently skipped. Identity now comes
   from the records, which already carry it on every line.

9. **MCP is streamable HTTP on the read listener** (owner decision), and the
   JSON-RPC surface is implemented directly rather than through an SDK — three
   methods over one POST endpoint did not justify a dependency tree, following
   the same instinct that kept DuckDB out of the runtime (D11).

10. **`diagnose()` on a healthy session returns a positive verdict** (owner
    decision, closing the §8 open question): the checks that passed, the
    signals that were unavailable, and any caveats. Returning nothing is honest
    but reads as a failure and gives a caller no way to tell a clean session
    from an analysis that never ran.

11. **The rule set is 15, not 14.** docs/13's fourteen rows plus
    `audio-overflow-latch` — docs/20 finding 8's concealment-vs-overflow ratio,
    which took a human noticing two counters together. Institutionalizing it is
    exactly what D6 says new rules are for.

12. **`gawk-telemetry` does not scale horizontally, and the chart refuses to
    try.** One writer per session file (D3) and one in-memory live projection;
    `replicaCount > 1` fails template rendering, and the strategy is `Recreate`
    so two pods never hold the same PVC.

13. **The app chart's default render is not byte-identical** — it gains one
    inert `"telemetryUrl": ""` line in `config.js`, which `getTelemetryUrl()`
    reads as unset. The relay chart's default render *is* byte-identical,
    asserted by diff against `HEAD`, and both are checked in CI.

14. **`relayCoverage` "full" requires two scrape intervals**, not one. A
    session sampled once cannot support a claim of full coverage even if the
    scrape happened to catch it.

15. **Native telemetry needs an explicit `-telemetry-url`.** The browser
    defaults to a same-origin path on the page it was served from; a native
    binary has only the relay origin, which is a different host by construction
    (the relay is a UDP LoadBalancer, the frontend an Ingress — the same reason
    `config.AppURL` exists separately). Unset means off.

16. **TM9 (Grafana) dropped**, per the owner's scope decision and the doc's own
    "TM9 is the droppable one". The §8 rollup-datasource question stays open.

17. **An R20 e2e pass was added** (`e2e/run.mjs --telemetry`), which the chunk
    table never asked for. Every other R28 test runs against fakes, in-memory
    sinks or handler-level calls — the same shape as the gap docs/24 finding 10
    found in R19, where "every carrier test ran against in-memory fakes or a
    zero-loss loopback, so a regression degrading carriers back to lossy
    delivery would ship green". The pass proves the pipe EXISTS: a real relay
    mints a real 0x0D onto a real uni stream, a real browser parses it and
    POSTs a real batch, and a real service verifies the token and stores a
    session `diagnose()` can answer about — with **the join** (the relay's
    `subscriberDetails[].sessionId` equalling the stored session's) as the
    headline assertion, because that is the gap R28 exists to close.
    Default-off is asserted on **every other** pass: the standard viewer run
    fails if it sees a single telemetry request.

### Defects the e2e pass found on its first run

All five were fixed at the source; none was reachable from the unit suite.

- **The CORS preflight could never reach the handler.** The ingest routes were
  registered method-scoped (`"POST /v1/ingest"`), and Go's `ServeMux` matches
  on method — so an `OPTIONS` preflight 404'd, the handler's own OPTIONS branch
  was unreachable dead code, and the browser blocked every cross-origin POST
  before sending it. Handler-level tests call `ServeHTTP` directly and pass
  with the bug present; the regression test is deliberately at the mux level.
  (Split-origin CORS itself was unimplemented until this pass needed it —
  D1 acknowledges the mode and nothing served it.)

- **A late batch duplicated the permanent rollup row.** With an idle timeout
  shorter than the client's flush interval, the sweep finalized a live session
  between its own flushes and every later batch re-created it: ONE viewer
  produced THREE rows. Duplicate rows corrupt every query over the permanent
  artifact (D4), so a finalized session now carries a tombstone for one idle
  timeout and a late batch is dropped and counted (`LateBatches`) rather than
  resurrecting it.

- **`relayCoverage` was never joined.** The rollup hardcoded `"none"`; TM4's
  join existed for `diagnose()`'s facts but was never written back to the row.
  That is not a missing field but an active lie — every verdict would be
  caveated as client-only even when the relay watched the whole session. The
  `Verdicter` hook now takes the row by pointer so the join happens before the
  verdict that reasons about it.

- **`diagnose()` was confidently wrong about a clean stream.** It fired
  `decoder-choking` on a healthy 30 fps loopback viewer because `factsFor` took
  the **median** of `receivedFps` and the **p05** of `decoderFps` — comparing
  "typical received" against "worst decoded", which any session with a warmup
  ramp fails. Both sides of a funnel ratio are now medians. This is exactly the
  §8 "verdicts that are confidently wrong" risk, and mixing statistics across a
  ratio is how you manufacture one.

- **Legitimate absence was counted as a data-quality anomaly.** `ViewerStats`
  declares dozens of fields as `T | null` (`avSkewMs` with no audio,
  `connection` because no browser ships `getStats()`, `renderedFps` on the
  main-thread path). Counting those as `dropped` gave a healthy 13-sample
  session 70 anomalies, which `distrustReason()` then reported as *"a client
  bug is likely"* — the false-alarm mirror of the wrong verdict above. A null
  is now **absence**: omitted from the stored object, counted as nothing. Only
  a wrongly-typed *non-null* value is an anomaly. Nested unknowns also count
  once rather than once per child.

Two smaller things the same run surfaced: the collector classified headless
Chrome as `"unknown"` (it reports `HeadlessChrome/`, never `Chrome/`), and four
`ReassemblerStats` counters were missing from the known-field table, arriving
as unknowns on every sample. Both fixed; a structural test now pins that every
field of that interface is typed.

## 5. Chunks and acceptance criteria

| Chunk | Scope | Acceptance criteria |
|-------|-------|---------------------|
| **TM1** | **Correlation ID**: `TypeTelemetryHello` (0x0D) + token mint/verify in `gawk-server/wire`; relay sends it on publish/subscribe (never `/internal/subscribe`); `subscriberDetails` gains `sessionId`; TS + `wirecheck` mirrors | Golden vectors byte-identical across Go/TS/`wirecheck`, incl. a strict-parse suite (wrong version/type/length, reserved flag bits set, expired token, tampered `broadcastKey`, tampered `role`); mint→verify round-trip and cross-fleet-key rejection; edge sessions provably receive none; `/statusz` gains `sessionId` additively with every existing key unchanged; no `telemetryKey` ⇒ `enabled: 0` and no behaviour change |
| **TM2** | **Client collectors**: browser viewer + broadcaster (main-thread, D13), native engine reporter, zero-PII envelope, batching/budget/beacon, R23 terms update + `termsVersion` bump | Unit tests: envelope carries obfuscated key + coarse browser/OS and **never** the raw UA or raw broadcast ID (asserted by grepping the serialized body); budget exhaustion degrades to events-only and records it; POST failure is silently dropped after N retries with **no** user-visible effect and no unbounded buffer; `enabled: 0` / no hello ⇒ zero network requests; jsdom test of the `visibilitychange` beacon path; **stats-tick timing unchanged** and no worker message added (asserted against the existing worker-protocol tests); terms page renders the new practice sentence; gawk-app gates green |
| **TM3** | **`gawk-telemetry` service**: module + image + chart (default off), ingest handler (token verify, D15 validation, size caps, per-IP rate limit without persistence), NDJSON writer, finalize + orphan sweep, 14-day prune | **Envelope strictness**: ingest rejects bad/expired/tampered token, oversize body, wrong `v`, role mismatch, malformed JSON, and a malformed `samples`/`events` *structure* — each with its own test and **no partial write**. **Payload tolerance (D15)**: a batch whose `stats` carries an unknown field is accepted and the field survives verbatim into the NDJSON; a wrongly-typed known field (string, `null`, non-finite) is dropped, counted, and does **not** reject the batch or reach any numeric series; a synthetic "next release" stats object with 20 new fields ingests cleanly against the current build (the skew case that motivates the whole decision). **Structural bounds** rejected independently of types (samples/events per batch, fields per object, nesting depth, string length), each with a boundary test. Plus: a valid batch lands as exactly N lines in the right partition; crash mid-session leaves a `.ndjson` that the startup sweep finalizes; prune deletes only partitions older than the window (property test around the boundary); **source IP appears in no file and no log line** (asserted over the whole data dir + captured logs); `helm template` with `telemetry.enabled: false` renders byte-identically to the pre-R28 chart |
| **TM4** | **Relay scrape + join**: headless companion Service in the relay chart, per-pod `/statusz` poll, relay records, `relayCoverage` | Fake multi-pod `/statusz` fixture (origin + edge, R17 shapes) produces one record per broadcast and per subscriber per pod; join by `sessionId` reconstructs a session's two-sided view; a session shorter than the interval yields `relayCoverage: "none"` and is never silently treated as full; a pod disappearing mid-scrape does not lose other pods' records; existing metrics Service + ServiceMonitor renders unchanged |
| **TM5** | **Rollups**: finalize-time computation, permanent row, additive-schema guarantee, `schemaAnomalies` | Scripted session → rollup row matches expected percentiles (incl. the "mean hides a freeze" case: a session with one 4 s stall must show it in p95 and in the stall fields); a rollup row from a synthetic *older* schema version still loads and queries; rollups survive a raw-partition prune (the point of the split). **Typed by construction (D15)**: every emitted numeric field is a finite number or **absent** — never a coerced guess, a `null`, or a zero standing in for "unknown" — asserted over a session whose samples are deliberately full of dropped/wrong-typed fields; that same session's `schemaAnomalies` counts match the ingest-side tally, and a session that hit its byte budget or has `seq` gaps says so on the row |
| **TM6** | **Read API + `diagnose()`**: HTTP JSON handlers, the docs/13 rule set, evidence provenance + confidence capping | Each transcribed playbook row has a test driving a synthetic session that fires it and one that does not; a client-only-evidence rule caps confidence (D7); relay/client disagreement surfaces as a finding rather than resolving silently; missing signals appear in `unavailable` instead of changing the verdict; **every default response ≤ 32 KB against a synthetic 4-hour session** (D10) |
| **TM7** | **MCP server**: the tools of §4.6 over the TM6 handlers, auth, docs | Each tool round-trips against a seeded store; the same query through HTTP and MCP returns the same data (one implementation, asserted); default bounds hold; `query_sql` is absent unless explicitly enabled; a documented end-to-end transcript: "diagnose yesterday's broadcast" from cold |
| **TM8** | **Live dashboard** (§4.8): in-memory live projection + `/live` endpoint, the three views, the shared-engine health model with hysteresis, embedded assets, separate non-public listener | **The headline criterion — an operator opening the fleet view during a staged-bad broadcast identifies the faulty stream, and which side is at fault, without clicking anything.** Plus: severity ordering puts the worst broadcast first (property test over generated fleets); a viewer whose client telemetry stops renders **stale**, and one that never reported renders **unknown** — neither ever `ok` (both asserted); the broadcaster and its viewers appear in one table with the broadcaster pinned first; live severity for a given window equals `diagnose()`'s verdict for the same window (one engine, asserted — they may not disagree); hysteresis holds (a single-sample blip does not escalate; a cleared fault persists through the dwell); relay-side and client-side freshness are labelled separately; served with no build step and **no external asset fetch** (asserted by loading with the network blocked); renders empty / live / degraded / relay-coverage-missing states; severity is never colour-only (glyph + label present in the DOM for each state); **the ended group renders below the live group and never interleaves — a live `warn` outranks an ended `bad`** — reads its verdict from the rollup index rather than recomputing, is recessed rather than hue-suppressed (an ended `bad` still shows red), and labels its claim in the past tense; the fleet view with zero live broadcasts shows recent history rather than an empty page; the read listener is absent from the public Ingress in `helm template` |
| **TM9** | *(droppable)* **Grafana dashboard** — closing R9 M8 | Dashboard JSON imports cleanly against a scraping Prometheus and every panel returns data during a test broadcast (M8's original criterion, unchanged); if the rollup-datasource question (§8) resolves negative, Prometheus-only panels ship and the finding is written back here |

Chunk prefix **TM** (two letters; A–Z claimed). Dependency spine:
TM1 → TM2/TM3 → TM4 → TM5 → TM6 → TM7/TM8 → TM9. Two chunks carry the
item's value and neither is droppable: **TM6** (`diagnose()`) is what makes it
work for the machine, **TM8** (the live dashboard) is what makes it work for a
human at 21:00 during a live stream. **TM9 is the droppable one** — trends can
wait, a stuttering broadcast cannot.

Every chunk follows [CODE-REVIEW.md](../CODE-REVIEW.md) — tests with the
change, bug fixes test-first.

---

## 6. Verification plan

Automated criteria are per-chunk above. Manual verify (the usual posture):

1. **End-to-end, single pod**: broadcast + 2 viewers with telemetry enabled;
   sessions appear under `sessions/`, rollups on close, `diagnose()` returns a
   plausible verdict for a *healthy* session (the boring case must not invent
   problems).
2. **Attribution drill, re-run against `diagnose()`**: stage the cheap
   playbook rows exactly as R9's verification did — throttle one viewer, set a
   low `-max-bandwidth`, force SW decode, briefly kill the broadcaster's
   uplink — and assert `diagnose()` reaches the same verdict the playbook's
   human procedure does. **This is the item's headline criterion.** Findings
   written back here.
3. **Cluster mode** (2 pods): per-pod scrape covers both, an origin/edge split
   session joins correctly, no double-counted subscribers.
4. **The AI loop, measured**: from a cold session, ask Claude to diagnose a
   staged-bad broadcast using only MCP. Criterion — **a correct verdict inside
   ~3 tool calls and well under the context a pasted blob costs today**. If it
   takes a raw dump to get there, TM6 is not done.
5. **The human loop, measured**: with a broadcast running and one viewer
   deliberately degraded (devtools throttle), open the dashboard cold on a
   phone or a second machine. Criterion — **the bad stream is identifiable,
   and attributable to the right side, from the fleet view alone, within
   seconds and without clicking**. Then repeat with the *broadcaster* degraded
   instead, and confirm the highlight moves to the broadcaster row rather than
   smearing across every viewer. If the operator has to drill in to find out
   something is wrong, TM8 is not done.
6. **Dashboard honesty**: kill one viewer's telemetry (block the ingest path
   in devtools) while it keeps watching happily, and confirm it reads
   **stale/unknown** rather than `ok` or `bad`; stop a broadcaster and confirm
   `away` is presented as a lifecycle state, not a fault.
7. **Privacy audit**: after a mixed session, grep the entire data directory
   and service logs for the raw broadcast ID, any full UA string, and any
   source IP. Zero hits, or the chunk is not done.
8. **Disabled-by-default**: an install with telemetry off shows no new
   requests from any client, no new Service, and no behaviour delta.

---

## 7. Non-goals

- **No media recording**, and nothing that makes R23's "no persistent media
  recording" statement untrue.
- **No PII**: no IP storage, no full UA strings, no cross-broadcast viewer
  identity, no fingerprinting (D8).
- **Not a Prometheus replacement.** Fleet health, live graphs and alert-shaped
  questions stay Prometheus's; R28 is per-session forensics beside it.
- **No OpenTelemetry / distributed tracing** — docs/13's stance stands.
- **No alerting or paging rules** for a hobby stream.
- **No hosted SaaS export.** Viewer telemetry stays in the homelab.
- **No sampling or opt-in gating** — always-on was the owner's decision, and
  opt-in reliably lacks data from the one viewer having the problem.
- **No new measurements.** If a chunk needs a counter that does not exist, it
  belongs to the milestone that owns that subsystem.

---

## 8. Risks and open questions

- **Context blowup** — the failure this item exists to prevent, reachable by
  building it carelessly. Mitigated structurally by D10 and asserted in TM6/
  TM7, because a habit is not a mitigation.
- **A new public write surface** on a deployment that today exposes only UDP
  4433 and static files. Mitigated by the stateless token (D2), body caps,
  non-persisting rate limits, and the fact that the worst case is junk in a
  prunable partition.
- **Verdicts that are confidently wrong.** The playbook was written for a
  human applying judgement; as code it will be believed. Mitigated by ranked
  candidates + evidence + provenance-capped confidence (D6/D7) — and it stays
  a risk.
- **Scrape-interval blindness** (D5): sub-5 s sessions have no relay side. A
  push path is the fix if it ever matters; it is deliberately not v1.
- **Storage growth** if the budget or prune is wrong. The arithmetic in §4.3
  is comfortable; a 60-viewer stress test is not, and the per-session budget
  is what bounds it.
- **Open: Grafana over rollups.** Prometheus panels (M8's original scope) are
  settled; rollup-driven panels would need a JSON/Infinity datasource against
  the read API. Deliberately unresolved until TM9 has the data to justify it.
  **TM9 was dropped from the R28 implementation (owner scope decision,
  2026-07-26)**, so R9 M8 stays open — trends can wait, and the built-in
  dashboard (TM8) covers the live question TM9 never would have.
- **The dashboard's health model is the same rules as `diagnose()` (§4.8.3),
  so a bad rule is now wrong in two places at once** — on a live page a human
  trusts at a glance, as well as in a verdict. That is the correct trade (two
  disagreeing truths would be worse), but it raises the cost of a sloppy rule,
  and it is why TM8's criteria assert the two agree rather than assuming it.
- ~~Open: what the fleet view shows when nothing is live.~~ **Resolved
  (owner, 2026-07-26): show recent ended broadcasts, distinguished by status
  and colour.** The live/history line the concern was about is kept by
  *grouping* rather than omission — a separate recency-sorted group below the
  live one, recessed in contrast, labelled in the past tense, and never
  interleaved, so a live `warn` always outranks an ended `bad` (§4.8.1,
  §4.8.4). Colour keeps carrying severity in hue; lifecycle rides contrast,
  which is what lets both be visible at once without competing.
- **Open: token lifetime vs very long broadcasts.** 24 h covers every
  plausible session; a broadcast outliving it would need a re-issued hello. A
  periodic re-hello is the obvious fix and is deferred until it is real.
- ~~**Open: what `diagnose()` should do with a session that is simply
  healthy.**~~ **Resolved (owner, 2026-07-26): a positive verdict with its
  basis.** The report carries `healthy: true`, the `passed` rule ids, the
  `unavailable` ones with the signal each was missing, and any caveats — so "no
  issues found" is distinguishable from "the analysis never ran", which is the
  same honesty rule TM8 applies to unknown/stale. `Severity()` returns
  `unknown` rather than `ok` when nothing could be evaluated at all.

---

## 9. Rejected alternatives

| Option | Why not |
|--------|---------|
| **Aggregates into the existing Prometheus** | Prometheus structurally cannot hold per-session detail — per-session labels are cardinality suicide (R9 D3 already says so). It answers "p95 viewer stall time", never "what happened to *that* viewer", which is half the ask |
| **Live-only ring in the relay, exposed via `/statusz`** | Smallest possible change and perfect correlation, but nothing survives the session — and telemetry riding the WebTransport session dies exactly when the transport is the problem (D1) |
| **ClickHouse / Grafana Faro + LGTM** | Wildly out of proportion at 5 broadcasts and 50 viewers; a real operational burden; docs/13 already rejected OTel for this system. Faro's SDK is web-vitals-shaped, so all the interesting instrumentation would still be written by hand |
| **Hosted SaaS (Grafana Cloud / Axiom / Honeycomb)** | Viewer telemetry leaves the homelab to a third party — against the self-hosted premise and a real question for third-party viewers. The client collector would still have to be built |
| **Embedding the store in the relay** | R17 made relay pods homogeneous, stateless and rollout-disposable. Giving one a database undoes that, and puts a storage failure domain inside the media hot path |
| **Relay pushes telemetry instead of being scraped** | Adds an outbound dependency and a queue to the process that carries every broadcast's hot path (D5). Revisit only if sub-scrape-interval sessions become diagnostically important |
| **SQLite instead of files** | Considered and offered; the owner chose files-first. NDJSON is directly readable by the primary consumer, needs no migrations, and makes retention a directory delete. SQLite's advantages (indexes, live dashboard queries) are not load-bearing at this volume |
