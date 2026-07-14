# R9 — Observability & Metrics: Design

Design doc for [ROADMAP R9](../ROADMAP.md#r9--observability--metrics).

The system already counts a lot — the relay's per-broadcast `hub.Stats`, the
broadcaster's `BroadcastStats`, the viewer's `ViewerStats` — but the counters
were accreted per-feature (C3, R4, R8) rather than designed as an
observability system. Three structural gaps make troubleshooting harder than
it should be:

1. **The relay's stats are unreachable by normal tooling.** `/statusz` and
   `/healthz` are served on the HTTP/3 mux — the server has **no TCP
   listener at all** (the Helm chart's probes exec `gawk-echo` for exactly
   this reason). Prometheus cannot scrape HTTP/3; even `curl` needs `--http3`.
   Nothing graphs over time, so "was that stutter a bandwidth spike or a
   subscriber queue overflowing?" is unanswerable after the fact.
2. **Nobody can attribute a problem to a network leg.** The path is
   broadcaster → relay → viewer, and every existing counter observes exactly
   one vantage point. The relay can't say "the broadcaster's uplink lost
   frames" (it has no ingress-loss signal — it only counts what *arrived*);
   the viewer can't distinguish "the relay never got it" from "my downlink
   dropped it"; neither client measures its own connection (RTT, packet
   loss, send-rate ceiling) even though `WebTransport.getStats()` exists.
3. **The broadcaster is flying blind in the production UI.** The viewer has
   a stats overlay (R6 J4); the broadcaster's production screen shows only
   the auto-ladder badges. The full encoder stats exist solely on the frozen
   `#/debug` page.

This doc designs a Prometheus-scrapeable relay, per-leg network health
signals on all three vantage points, a unified client stats model with
overlays on **both** surfaces, and a bottleneck playbook that maps symptoms
to metric signatures — so a stutter report from a friend (or a Claude
debugging session) starts from data, not vibes.

## Goals

- **Prometheus-compatible `/metrics` on a non-public endpoint**: a separate
  plain-TCP HTTP listener (own port), exposed in-cluster only via a
  ClusterIP Service + ServiceMonitor — never on the public LoadBalancer.
- **Per-leg bottleneck attribution**: for any playback problem it must be
  possible to decide, from metrics alone, which of these is the culprit:
  broadcaster encode, broadcaster→relay network, relay egress/bandwidth cap,
  relay→viewer network, viewer decode, viewer render.
- **Pipeline funnel rates at every stage**: captured fps → post-gate fps →
  encoded fps → **actually-sent fps** on the broadcaster; received fps →
  decoded fps → rendered fps on the viewer. A rate drop between two adjacent
  stages localizes the bottleneck to that stage.
- **Both clients get a stats overlay** in the production UI, same
  interaction model as the viewer's today (hotkey + right-click), fed by a
  unified stats shape — plus one-click "copy diagnostics as JSON" so a
  friend can paste their view of the world into a chat.
- **Useful to the machine, not just the human**: metric names, label
  semantics, and the symptom→signature playbook live in this doc so a future
  troubleshooting session can go from "viewer X stutters" to the
  discriminating query without re-deriving the model.
- Every new knob plumbed **all the way to production** (flag → env → Helm
  values → Deployment args), with an acceptance criterion that checks it —
  the R2 post-implementation lesson.

## Non-goals

- **Client→server metrics push.** Browser clients can't be scraped, and
  shipping their stats to the relay (to re-export via Prometheus) is a new
  protocol surface + retention story for marginal gain at 15 viewers. The
  copy-diagnostics-JSON button covers the "get me that viewer's numbers"
  case. Deferred, not rejected — revisit if remote troubleshooting of
  friends' sessions becomes routine.
- **True glass-to-glass latency.** Frame timestamps are relative to the
  broadcaster's clock; measuring capture→render across machines needs a
  clock-sync story. That is R5's live-edge measurement work — this design
  provides per-leg *component* latencies (encode latency, RTT/2 per leg,
  queue depths, decode latency) that bound it, and R5 slots the real number
  into the same overlay row later.
- **Distributed tracing / OpenTelemetry.** One binary, one hop — Prometheus
  counters and two overlays are the right weight. No OTel SDK.
- **Alerting rules.** A homelab Grafana dashboard is provided (M8); paging
  rules for a hobby stream are not.
- **Frontend (gawk-app) server metrics.** The nginx pod serves static files;
  its health is not a streaming bottleneck. Out of scope.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| M1 | **Ops HTTP listener** — new plain-TCP `net/http` server on `-metrics-addr` (default `:2112`, `off` disables; `GAWK_METRICS_ADDR`), serving `/metrics` (client_golang handler: Go + process collectors, `gawk_build_info`), `/healthz`, and a mirror of `/statusz` (same JSON as the H3 route) | Unit tests: listener serves all three routes over TCP; empty addr starts no listener; `gawk_build_info{version}` carries the ldflags version; H3 `/statusz` unchanged (existing tests green); startup log includes the metrics addr | ✅ done (`internal/ops`; the H3 route now shares `ops.StatuszHandler`) |
| M2 | **Registry collector** — `internal/metrics` custom `prometheus.Collector` snapshotting `Registry.Stats()` into per-broadcast + total const-metrics; hub counter refinements: split `keyframeStreamsDropped` by reason (`superseded`/`slow`/`bandwidth`/`open_failed`), surface the existing-but-unread `Subscriber.sendErrors`, add egress-bytes counters (datagram + keyframe bytes actually written) | Unit tests with `prometheus/testutil`: metric names/labels/values match a scripted hub scenario (drops attributed to the right reason label); `/statusz` JSON gains the new fields additively (existing keys unchanged); collector holds the registry lock only for the snapshot | ✅ done (`internal/metrics`; see the naming note under the inventory) |
| M3 | **Ingress loss window** — RTP-style sliding window (~1024 frameIDs, per publisher generation) over VideoChunk/StreamFrame headers in the hub: a frameID that ages out unseen counts `frames_lost`; per-frame distinct-chunks-seen vs `chunkCount` counts `chunks_lost`; both per broadcast. Plus per-subscriber detail in `/statusz` (hashed key, live queue depth, dropped, sendErrors, keyframes sent/dropped) | Unit tests: scripted ingest with gaps/reorder/wraparound counts losses correctly (out-of-order arrival within the window is *not* a loss; publisher restart resets the window); memory bounded regardless of frameID jumps; `/statusz` lists one entry per live subscriber with a stable-within-session hashed key | ✅ done (`internal/hub/ingress.go`; detail array is `subscriberDetails`, keys are random per-session) |
| M4 | **Route & limit counters** — `gawk_connections_total{route,outcome}` in the transport handlers (publish/subscribe/echo × accepted/unauthorized/not_found/conflict/limit_rejected/upgrade_failed), `gawk_rate_limited_total`, `gawk_origin_rejected_total` | Unit tests drive each handler rejection path (the transport tests already exercise them) and assert the labeled counter increments; accepted sessions count exactly once | ✅ done (`outcomes_test.go` drives rejections pre-upgrade with httptest; accepted asserted in the e2e relay test) |
| M5 | **Helm: metrics Service + ServiceMonitor** — new ClusterIP Service exposing only the metrics TCP port; `servicemonitor.yaml` gated by `metrics.serviceMonitor.enabled` (default false — needs prometheus-operator CRDs) with configurable labels/interval/scrapeTimeout; values `metrics.{enabled,port}`; Deployment gains the container port + `--metrics-addr` arg; metrics port **never** added to the public LoadBalancer Service | `helm template` snapshots: ServiceMonitor rendered only when enabled and selects the metrics Service by label; LoadBalancer Service unchanged; disabling `metrics.enabled` removes port/args/Service/ServiceMonitor; chart lints | ✅ done (render-checked default / SM-enabled / disabled; disabled passes `GAWK_METRICS_ADDR=off`) |
| M6 | **Client connection + funnel stats** — sample `WebTransport.getStats()` (feature-detected, null-safe) each stats tick in both pipelines (viewer: inside the worker); extend `BroadcastStats` (captureFps, sentFps, connection substats) and `ViewerStats` (receivedFps, renderedFps, timeSinceLastFrameMs, lastKeyframeAgeMs, connection substats); RenderSink gains a drawn-frames counter | Unit tests with a fake `wt.getStats` (present, absent, and rejecting): stats carry rtt/loss/send-rate when available and nulls otherwise, never throwing; funnel counters advance in a scripted pipeline run (sent counts only *successful* sends); worker path forwards the new fields through `ViewerWorkerOutbound` unchanged | ✅ done (`transport/net-stats.ts` sampler; renderedFps is null on the main-thread path by design) |
| M7 | **Unified overlays + diagnostics export** — generalize the R6 overlay into a shared sectioned component (Video / Pipeline / Network) computing windowed rates from successive samples; broadcaster production screen gets the overlay (same hotkey family as the viewer's); viewer overlay gains the new rows; both get "Copy diagnostics" (JSON: surface, UA, settings, last N stat samples). `#/debug` pages stay frozen and untouched | Component tests: overlay renders full and degraded (nulls → "—") stats; copy button writes well-formed JSON including config + samples; broadcaster hotkey toggles; existing viewer overlay tests keep passing; tsc/lint/build green | ✅ done (shared `ui/StatsPanel` + `lib/diagnostics`; `STATS_HOTKEY` moved to `lib/hotkeys.ts`) |
| M8 | *(optional)* **Grafana dashboard + QUIC tracer** — `gawk-server/deploy/grafana/gawk-relay.json` (funnel, per-leg loss, bandwidth vs cap, per-broadcast drill-down); investigate quic-go `logging.Tracer` for server-side RTT/loss histograms (API needs verification; aggregate-only labels to bound cardinality) | Dashboard imports cleanly against a scraping Prometheus and every panel query returns data during a test broadcast; tracer metrics (if the API pans out) covered by an integration test, else the finding is written back into this doc | deferred — needs the live homelab Prometheus/Grafana that the manual verify uses; do it alongside (or after) manual verification |

Chunk letters continue the existing sequence (A–J, S used); R9 takes **M**.
Ordering: M1→M2→M3 are a dependency chain on the server; M4 and M5 need only
M1; M6→M7 are client-side and independent of the server chunks. Every chunk
follows CODE-REVIEW.md (tests with the change; bug fixes test-first).

**M1–M7 implemented 2026-07-14**; all automated gates green (gofmt/vet/
`go test -race`, vitest/lint/`tsc -b` build, helm lint + template renders).
The [manual verification plan](#verification-plan) — cluster scrape path,
attribution drills, browser overlays — **passed 2026-07-14**.
M8 (Grafana dashboard + optional QUIC tracer) remains deferred (needs
dedicated time with the live homelab Prometheus/Grafana).

## Current state (inventory)

What exists today, and what each surface can and cannot answer:

| Surface | Lives in | Has | Blind spots |
|---------|----------|-----|-------------|
| Relay `/statusz` | `hub.Stats` / `RegistryStats`, served on the H3 mux | Per-broadcast + totals: frames/datagrams relayed, per-sub queue drops, bad datagrams, bandwidth drops, keyframe stream in/sent/dropped/oversize, cache state, grace | HTTP/3-only (unscrapeable); point-in-time only (no rates/history); keyframe drops lump 4 causes into one number; `sendErrors` counted but never exposed; **no ingress-loss signal** (can't see broadcaster→relay loss); no per-subscriber breakdown |
| Broadcaster `BroadcastStats` | `transport/broadcaster.ts` → `BroadcasterScreen` badges / debug `StatsGrid` | encodedFrames/keyframes, droppedFrames (encoder busy), fpsGateDropped, datagrams/bytes sent, keyframe streams sent/failed, encoderQueueDepth/Fps, encode latency, R4 auto-ladder state | No capture fps (funnel start), no **sent fps** (frames, not datagrams, that actually left), no connection health (RTT/loss/send-rate/at-capacity), production UI shows almost none of it |
| Viewer `ViewerStats` | `transport/viewer.ts` (+ reassembler/reorder stats) → `StatsOverlay` | datagrams received/bad/dup, frames completed/dropped-incomplete/late, decoded frames + fps + queue depth + latency, awaiting-keyframe discards, HW/SW decode, keyframe streams received, reorder gap-resyncs/buffered | No received-fps or rendered-fps rates, no connection health, no time-since-last-frame or keyframe-age (stall visibility), cumulative counters shown raw (no windowed rates) |
| Loopback `PipelineStats` | `media/types.ts` | encode+decode side incl. `lastEndToEndLatencyMs` (same-machine clock, so it *can* measure e2e) | Debug-only surface; fine as-is, untouched by R9 |

## Design decisions

### D1 — A second, plain-TCP HTTP listener for ops

The WebTransport server is HTTP/3-over-UDP and must stay that way; Prometheus
speaks HTTP/1.1 over TCP. So the relay grows a second listener: a stock
`net/http` server on `-metrics-addr` (default `:2112`, the client_golang
convention; empty string disables). It serves:

- `GET /metrics` — Prometheus text format (client_golang `promhttp`).
- `GET /healthz` — trivial liveness (note: the k8s probes deliberately stay
  on the exec-`gawk-echo` path — QUIC reachability is the service; a TCP 200
  proves much less. The TCP healthz exists for humans and future use).
- `GET /statusz` — the same JSON snapshot as today, mirrored here so it is
  finally `curl`-able without an HTTP/3 client. The H3 route stays (it costs
  one shared handler and existing tooling may use it).

The listener binds all interfaces inside the pod but is only reachable
through the new ClusterIP Service (D6) — the public LoadBalancer Service
forwards UDP 4433 only and is not touched. No auth on `/metrics`: it is
cluster-internal, and broadcast IDs appear only in obfuscated form (D3).

### D2 — Snapshot collector over `Registry.Stats()`, not parallel bookkeeping

The hub already maintains exactly the counters we want, with careful
fold-on-close semantics (live subscriber atomics summed on demand, folded
into hub/registry totals on close/GC). Duplicating those into
`prometheus.Counter`s would create two books to keep consistent across every
code path — the classic drift bug.

Instead, a new `internal/metrics` package implements a custom
`prometheus.Collector` whose `Collect` calls `Registry.Stats()` and emits
`ConstMetric`s from the snapshot. One source of truth; the hub stays free of
Prometheus imports; `/statusz` and `/metrics` can never disagree. The
snapshot holds the registry lock exactly as `/statusz` does today — same
cost, ~1 scrape per 15 s.

Monotonicity caveat: per-broadcast counters vanish when a broadcast is GC'd
(folded into totals). Per-broadcast series therefore disappear rather than
reset — normal Prometheus lifecycle for labeled series — and the **totals**
metrics (which only ever grow) are the ones to use for long-range rate
queries. The playbook queries below respect this.

Things the snapshot can't see — per-request outcomes (M4) and ingress-loss
events (M3) — are instrumented directly at their call sites; they are new
counters with no existing book to drift from.

### D3 — Label scheme and cardinality

- `broadcast` — the **obfuscated** broadcast ID (the existing per-process
  HMAC via `ObfuscateID`; raw IDs are joinable and must never appear in
  metrics, same rule as `/statusz`). Bounded by `MaxBroadcasts` (5), so
  cardinality is trivial. Two accepted quirks, documented here: label values
  are ephemeral (new ID per broadcast session) and change across server
  restarts (fresh HMAC key) — fine for a homelab retention window, and the
  totals series carry the long-range view.
- `reason` — closed enums per metric (e.g. keyframe drops:
  `superseded|slow|bandwidth|open_failed`; datagram drops:
  `queue_full|bandwidth`). `superseded` is *benign* (a newer keyframe
  replaced an in-flight one); separating it out is precisely what makes the
  drop counter diagnostic instead of noise.
- `route`/`outcome` — closed enums on connection counters (M4).
- **No per-subscriber labels.** 50 ephemeral subscribers × churn would be
  pointless series bloat; per-subscriber detail goes into `/statusz` JSON
  (M3) where it belongs — a live debugging view, not a time series. The
  hashed per-subscriber key is stable within a session so a slow viewer can
  be watched across refreshes of `/statusz`.

### D4 — Per-leg attribution: three vantage points + an ingress-loss window

The central design idea. Loss/bottleneck attribution needs measurements on
both ends of each leg:

```
 broadcaster ──(leg A)──► relay ──(leg B)──► viewer
 encode/send             ingress/egress      receive/decode/render
```

- **Leg A, client end**: the broadcaster samples `WebTransport.getStats()` —
  smoothed RTT, packets lost, `estimatedSendRate`, `atSendCapacity`, and
  datagram-specific `expiredOutgoing`/`lostOutgoing` (frames that died in the
  local send queue vs on the wire). This is the "is my uplink the problem"
  view, visible in the broadcaster overlay.
- **Leg A, relay end (new, M3)**: the relay currently counts only what
  arrives. An RTP-style sliding window over frameIDs (both datagram chunk
  headers and keyframe streams feed it) counts a frame as lost only when it
  ages out of the window unseen — robust to QUIC datagram reordering, unlike
  naive gap counting. Per-frame `chunkCount` vs distinct chunks seen
  additionally measures *partial* frame loss. `rate(gawk_relay_ingress_frames_lost_total)`
  **is the leg-A loss rate as the relay experienced it** — the single most
  clarifying new server metric.
- **Leg B, relay end (exists, sharpened)**: per-subscriber queue-overflow
  drops, keyframe write-timeouts (`reason="slow"`), send errors, and
  bandwidth-cap drops — now with reasons split and per-subscriber detail in
  `/statusz`. High drops for *one* subscriber = that viewer's downlink; high
  for *all* = relay egress (or the configured cap — `reason="bandwidth"`
  makes that unambiguous).
- **Leg B, viewer end**: `getStats()` RTT/loss plus the existing
  reassembler counters, and new `receivedFps` / `timeSinceLastFrameMs` /
  `lastKeyframeAgeMs` for stall visibility.

Cross-referencing is the operator's (or Claude's) job via the playbook — the
design deliberately does *not* build a channel that shows viewer-side stats
to the broadcaster or vice versa (see Non-goals).

### D5 — The funnel: rates at every pipeline stage

Cumulative counters answer "how many ever"; bottleneck hunting needs "how
many per second, right now, at each stage". The unified stats model defines
the funnel stages and the overlays compute windowed rates from successive
samples (the stats tick is already 500 ms on both pipelines):

- **Broadcaster**: `captureFps` (frames handed to the preprocessor) →
  post-gate fps (capture − `fpsGateDropped` rate) → `encoderFps` (exists) →
  **`sentFps`** (frames whose datagrams/stream were actually written without
  error — the "actually sent framerate" the requirements ask for).
- **Viewer**: `receivedFps` (complete frames: reassembled datagram frames +
  keyframe streams) → `decoderFps` (exists) → `renderedFps` (RenderSink
  draws; on the main-thread fallback path, counted at the draw callback).

Adjacent-stage gaps localize: capture−gate gap is intentional (the fps
rung); gate→encode gap = encoder overload (R4's signal); encode→sent gap =
send-path backpressure; sent→received gap = network legs (split via D4);
received→decoded gap = decoder choking; decoded→rendered gap = render/tab
throttling.

### D6 — Kubernetes exposure: ClusterIP Service + ServiceMonitor

A new `templates/service-metrics.yaml` (ClusterIP, the metrics TCP port
only, labeled for selection) and `templates/servicemonitor.yaml` gated by
`metrics.serviceMonitor.enabled` (default **false** — the CRD only exists
where prometheus-operator is installed; the homelab runs kube-prometheus-
stack, which also needs its `release` label on the ServiceMonitor —
configurable via `metrics.serviceMonitor.labels`). Values:

```yaml
metrics:
  enabled: true          # ops listener + metrics Service
  port: 2112
  serviceMonitor:
    enabled: false       # requires prometheus-operator CRDs
    labels: {}           # e.g. {release: kube-prometheus-stack}
    interval: 15s
    scrapeTimeout: 10s
```

A PodMonitor would avoid the extra Service but a Service is more portable
across scrape setups and doubles as a stable in-cluster DNS name for ad-hoc
`curl`/port-forward debugging. The public LoadBalancer Service is untouched;
that is what keeps the endpoint non-public.

### D7 — `WebTransport.getStats()` is feature-detected, never assumed

The spec defines it and Chromium ships it; Firefox support is doubtful and
the returned dictionary's field set varies by version. Both pipelines treat
it as optional: probe `typeof wt.getStats === 'function'`, sample inside a
try/catch each stats tick, map missing fields to `null`, and the overlays
render `—`. The exact field availability per browser is a manual-verify
item (M6/M7 verification), not an assumption baked into types — all
connection fields are nullable. On the viewer this sampling runs **inside
the worker** (the `WebTransport` lives there); the fields ride the existing
`ViewerWorkerOutbound` stats event, so no new worker plumbing.

### D8 — Diagnostics export for remote troubleshooting

Each overlay gets a "Copy diagnostics" action: a JSON blob with surface
(broadcaster/viewer), app version, user agent, active settings (ladder rung,
codec, worker vs main-thread path), and the last ~20 stat samples
(10 seconds of funnel + connection history — enough to see a transient).
That makes "friend pastes a blob into the group chat" the remote-debugging
story, in place of the rejected client-metrics-push channel. It is also the
designed entry point for Claude-assisted debugging of a remote viewer.

## Metric inventory (server, `/metrics`)

Names follow Prometheus conventions (`_total` counters, base units, no
rates — rates are query-side). Every data-plane counter exists as **two
families**: `gawk_broadcast_*{broadcast=<obfuscated id>}` (per-broadcast
drill-down; series vanish when the broadcast is GC'd) and `gawk_relay_*`
(registry-lifetime totals including folded/expired broadcasts — the ones for
long-range rate queries). *Implementation note*: the design originally
sketched one family emitting both labeled and unlabeled series, but
client_golang rejects a single metric name with inconsistent label
dimensions — the two-prefix split is the clean expression of the same idea.

| Metric (`gawk_broadcast_*` and `gawk_relay_*`) | Type | Extra labels | Answers |
|--------|------|--------|---------|
| `…_frames_relayed_total` | counter | `kind=delta\|keyframe` | relay throughput in frames; `rate()` ≈ relay-side fps |
| `…_ingress_frames_lost_total` | counter | — | **leg-A loss**: frames the broadcaster sent that never reached the relay (window-aged, M3) |
| `…_ingress_chunks_lost_total` | counter | — | leg-A partial loss (missing chunks of seen frames) |
| `…_ingress_bytes_total` | counter | `kind` | broadcaster→relay volume |
| `…_egress_bytes_total` | counter | `kind` | relay→viewers volume actually written; graph against the cap |
| `…_datagrams_relayed_total` | counter | — | fan-out datagram volume (pre-drop) |
| `…_datagrams_dropped_total` | counter | `reason=queue_full\|bandwidth` | **leg-B pressure**: slow-viewer queue overflow vs configured cap |
| `…_send_errors_total` | counter | — | datagram write failures to viewers (was counted, never exposed) |
| `…_bad_datagrams_total` | counter | — | malformed publisher input |
| `…_bandwidth_dropped_bytes_total` | counter | — | bytes the egress cap discarded (datagrams + keyframes) |
| `…_keyframe_streams_in_total` | counter | — | reliable keyframe ingest; `rate()` ≈ GOP cadence check |
| `…_keyframe_streams_sent_total` | counter | — | keyframes fully delivered |
| `…_keyframe_streams_dropped_total` | counter | `reason=superseded\|slow\|bandwidth\|open_failed` | keyframe delivery health; `slow` = stalling viewer, `superseded` = benign |
| `…_keyframe_streams_oversize_total` | counter | — | publisher exceeding `MaxKeyframeBytes` |

Per-broadcast gauges and server-wide metrics:

| Metric | Type | Labels | Answers |
|--------|------|--------|---------|
| `gawk_build_info` | gauge (1) | `version` | what's deployed |
| `gawk_broadcasts_active` | gauge | — | registry occupancy vs `MaxBroadcasts` |
| `gawk_subscribers_active` | gauge | — | audience size vs limits (all broadcasts) |
| `gawk_broadcast_publisher_active` | gauge (0/1) | `broadcast` | is the broadcaster connected or in grace |
| `gawk_broadcast_grace_remaining_seconds` | gauge | `broadcast` | time until GC while away |
| `gawk_broadcast_subscribers` | gauge | `broadcast` | audience size per broadcast |
| `gawk_broadcast_cached_keyframe_bytes` | gauge | `broadcast` | late-joiner prime size (also a bitrate proxy) |
| `gawk_connections_total` | counter | `route`, `outcome` | connect/reject rates per route (M4); outcomes: `accepted`, `unauthorized`, `not_found`, `conflict`, `limit_rejected`, `upgrade_failed`, `error` |
| `gawk_rate_limited_total` | counter | — | per-IP limiter hits (counted here only, not as a connection outcome) |
| `gawk_origin_rejected_total` | counter | — | misconfigured `allowedOrigins` / probing |
| Go/process defaults | — | — | CPU, RSS, goroutines, GC — free from client_golang |

`/statusz` additions (JSON, additive): `keyframeDrops`
(`superseded`/`slow`/`bandwidth`/`openFailed`), `sendErrors`,
`ingressDatagramBytes`, `egressDatagramBytes`, `egressKeyframeBytes`,
`ingressFramesLost`, `ingressChunksLost`, and a `subscriberDetails` array
per broadcast (`key` — random per-session, `queueDepth`, `dropped`,
`sendErrors`, `keyframesSent`, `keyframesDropped`). One sharpened semantic:
`bandwidthDroppedDatagrams` now counts *datagram* bandwidth drops only
(keyframe bandwidth drops moved to their own reason counter — previously a
keyframe drop inflated the datagram counter, which would have made
queue-overflow-by-subtraction go negative); `bandwidthDroppedBytes` still
covers both kinds.

## Client stats additions

All new fields nullable; `— ` in overlays when unavailable.

**`BroadcastStats` (broadcaster)**: `captureFps`, `sentFps` (see D5),
`keyframeBytesSent`; `connection: { rttMs, rttVarMs, packetsSent,
packetsLost, bytesSent, estimatedSendRateBps, atSendCapacity,
datagramsExpiredOutgoing, datagramsLostOutgoing }` from `getStats()`.

**`ViewerStats` (viewer)**: `receivedFps`, `renderedFps`,
`timeSinceLastFrameMs` (stall detector), `lastKeyframeAgeMs` (recovery-bound
indicator: should hover ≤ GOP length), `bytesReceived`; `connection:
{ rttMs, rttVarMs, packetsReceived, packetsLost, datagramsDroppedIncoming }`.

Existing fields are kept as-is (cumulative); the shared overlay component
derives windowed rates client-side from successive samples rather than
changing counter semantics under the debug pages.

## Bottleneck playbook

The point of the whole design: symptom → discriminating signals → verdict.
"Overlay" = the client stats overlay; server signals are Prometheus queries
(or `/statusz` deltas).

| Symptom | Discriminating signals | Verdict |
|---------|------------------------|---------|
| One viewer stutters, others fine | That subscriber's `queueDepth`/`dropped`/keyframe `slow` drops high in `/statusz` while others ≈ 0; viewer overlay shows rising RTT / `packetsLost` / `datagramsDroppedIncoming`; relay ingress loss ≈ 0 | **Leg B for that viewer** (their downlink or machine's network) |
| All viewers stutter; relay ingress loss ≈ 0; egress drops `reason="bandwidth"` climbing | `gawk_relay_datagrams_dropped_total{reason="bandwidth"}` / keyframe `bandwidth` drops > 0 | **Configured bandwidth cap** — raise `-max-bandwidth` or lower the ladder rung |
| All viewers stutter; relay ingress loss ≈ 0; `queue_full` drops on *all* subscribers | Uniform per-subscriber drops; node/egress saturation (check node network metrics) | **Relay egress / homelab uplink** |
| All viewers stutter; relay ingress loss **rising** | `rate(gawk_relay_ingress_frames_lost_total)` > 0; broadcaster overlay: `atSendCapacity`, `estimatedSendRate` < configured bitrate, `datagramsLostOutgoing`/`expiredOutgoing` rising | **Leg A** — broadcaster uplink; drop a rung or bitrate |
| Broadcaster: choppy source, no network signals | Overlay funnel: post-gate fps healthy but `encoderFps` below it; `encoderQueueDepth` growing; `droppedFrames` rising; (R4 `encoderPressure` badge in explicit mode) | **Encoder overload** — the R4 auto ladder's territory; on HW encode paths watch the funnel gap, since `encodeQueueSize` under-fires there (docs/09 finding) |
| Broadcaster: encodes fine, viewers see low fps | `encoderFps` healthy, `sentFps` below it — or `sentFps` healthy but relay `rate(frames_relayed)` below it (→ leg A) | **Send path vs leg A** — the funnel splits it |
| Viewer: fps low, network clean | `receivedFps` healthy but `decoderFps` below it; `decoderQueueDepth` high; `reorderGapResyncs` climbing; `isHardwareAccelerated: false` | **Decoder choking** (likely SW-decode fallback) — lower the rung/fps |
| Viewer: smooth then freezes | `timeSinceLastFrameMs` grows while connection RTT still updates → upstream stopped (check `gawk_broadcast_publisher_active`); RTT also dead → leg B outage (reconnect logic's territory) | **Stall attribution** |
| Frequent "Awaiting keyframe" / gap resyncs on one viewer | Viewer `framesDroppedIncomplete`/`reorderGapResyncs` up; relay keyframe `slow` drops for that subscriber; `lastKeyframeAgeMs` spiking ≫ GOP | **Delta loss on leg B** eating GOPs; keyframe cadence + reliable streams bound recovery — if age ≫ 500 ms GOP something is wrong at the relay |
| Nothing plays for anyone | `gawk_connections_total{outcome!="accepted"}` — 401 (secret), 404 (bad/expired ID), 429 (limits/rate limiter), `origin_rejected` (CORS config) | **Config/limits, not media** |

## Verification plan

Automated per-chunk criteria are in the Status table. Manual verify (the
usual posture — browser + cluster behavior can't be fully automated here):

1. **Scrape path**: deploy to the homelab; ServiceMonitor enabled → target
   appears healthy in Prometheus; every metric in the inventory table
   returns data during a test broadcast; `/metrics` is *not* reachable from
   outside the cluster (LoadBalancer still UDP-only).
2. **Attribution drill**: induce each playbook row that's cheap to stage —
   throttle a viewer (devtools) → that subscriber's leg-B signature; set a
   low `-max-bandwidth` → `reason="bandwidth"`; force SW decode → decoder
   signature; kill the broadcaster's uplink briefly → ingress-loss counter
   moves. Confirm the playbook's verdicts hold; write findings back here.
3. **Overlays**: Chrome + Firefox, broadcaster + viewer — funnel rates
   plausible against a wall clock, `getStats` fields present on Chromium and
   gracefully `—` where unsupported, copy-diagnostics JSON parses and
   contains samples.

## Risks & open questions

- **`WebTransport.getStats()` field variance** (D7): shipped shape differs
  across Chromium versions; Firefox likely absent. Mitigated by all-nullable
  fields + manual verify matrix. Worst case the connection section is
  Chromium-only — the funnel and server metrics don't depend on it.
- **quic-go tracer API** (M8): server-side RTT histograms depend on
  `logging.Tracer` details that need verification against the pinned
  quic-go version; deliberately optional and last.
- **Ingress window tuning** (M3): 1024 frames ≈ 17–34 s at 30–60 fps; late
  arrivals beyond that count as losses (they'd be useless for playback
  anyway). Constant named and documented next to the reassembler's
  equivalents.
- **Collector lock hold** (D2): `Registry.Stats()` under scrape every 15 s
  is the same cost as today's `/statusz` polling; if per-subscriber
  summation ever grows hot, snapshot copying can be optimized without
  changing the exposition.
