# R59 — Usage and capacity metrics

**Status**: done. Designed 2026-09-29 with the owner (decisions OD1–OD7,
§2); chunks **UM1–UM6** (§5). UM1–UM4 implemented 2026-09-29, UM5 deployed
and UM6 built on the reference deployment 2026-09-30 (§6).

**Relationship to earlier work**: R9 (docs/13) made the relay scrapeable and
gave it the per-leg counters. Its M8 was a Grafana dashboard checked in at
`gawk-server/deploy/grafana/gawk-relay.json`, deferred since 2026-07-14.
This milestone replaces M8: the dashboard exists, but lives in Grafana
rather than the repo (OD1). What it could not show from the existing series
is what this milestone adds.

---

## 1. Why, and what "done" means

The production dashboard (`gawk.ioio.fi` on `grafana.ext.ioio.fi`) was one
panel of ingress request counts. On 2026-09-29 it was rebuilt from the
existing series into usage, capacity, per-component CPU and memory, a daily
quality trend and the surrounding services. Building it showed what the
series cannot answer:

| Question | Why the existing series can't answer it |
|---|---|
| How many broadcasts started today? | `gawk_connections_total{route="publish",outcome="accepted"}` counts a new broadcast and every reconnect the same way |
| How many people watched? | `subscribe` accepted counts sessions, including automatic reconnects and R30 stripe legs |
| How long do broadcasts and watches last? | Nothing records a duration; per-broadcast series vanish at GC |
| How many viewers did a broadcast peak at? | Only a live gauge, gone when the broadcast ends |
| Which clients, codecs and resolutions are used? | The relay records none of it |
| How close are we to the caps? | The caps exist only as env vars, so panels hardcode them |
| Is `gawk-admin` healthy? Is its database? | `gawk-admin` has no `/metrics`; CNPG's exporter isn't scraped |
| Is `gawk-telemetry` healthy? | Its `/metrics` exists but prod has no ServiceMonitor for it |

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| Broadcasts started per day counts new broadcasts, not reconnects | `TestBroadcastsStartedCountsMintAsNewAndClaimAsResumed` (UM1) |
| Viewer joins exclude stripe legs, internal pulls and automatic reconnects | `TestViewerJoinsCountPrimariesOnly` (UM1); `viewer-session.test.ts` rejoin test (UM2) |
| Broadcast duration, peak viewers and watch time are recorded when they end | `TestBroadcastEndObservesDurationAndPeak`, `TestBroadcastEndForcedAndEdge`, `TestViewerJoinsCountPrimariesOnly` (UM1) |
| The client mix is visible with bounded labels | `TestClientLabelsNormalize` (UM1); URL tests in UM2 and UM3 |
| Codec and resolution come from the media, not a client claim | `TestMediaInfoFromH264SPS`, `…VP8`, `…VP9`, `TestHubProbesCodecAndHeightFromMedia` (UM1) |
| Every cap is a metric | `TestLimitGaugesExportConfiguredCaps` (UM1) |
| The client mix is exported per origin broadcast, never by edges | `TestBroadcastInfoGaugeLabels`, `TestBroadcastInfoGaugeOriginOnly` (UM1) |
| `gawk-admin` is scrapeable on a listener that is never routed publicly | UM4 tests; `helm template` shows no metrics port on the Ingress |
| No new label can carry a raw broadcast ID, an IP, a user agent or a location | code review against §3 D1 and D6; the collector tests assert the label sets |
| The dashboard's usage rows read the new series in production | UM6: every new panel returns data after the release deploys |

---

## 2. Owner decisions (2026-09-29)

- **OD1 — The dashboard lives in Grafana, edited in its UI.** No JSON in the
  repo, no provisioning ConfigMap. This drops R9 M8's
  `gawk-server/deploy/grafana/gawk-relay.json`. Self-hosters get the metric
  names from this doc and docs/13.
- **OD2 — What it's for: capacity and trends, and product usage.** Not live
  operations and not per-incident forensics; per-session forensics stays in
  `gawk-telemetry` (R28).
- **OD3 — Uplink use is relay egress against a fixed 1 Gbit/s line.** No
  router SNMP. Egress includes origin-to-edge traffic inside the cluster, so
  it overstates uplink use when a broadcast spans pods; the panel says so.
- **OD4 — Close the metric gaps in code**, not only in the dashboard: the
  counters in §3, the caps as metrics, and scraping for `gawk-admin`,
  `gawk-telemetry` and the admin database.
- **OD5 — 30 days of history is enough.** Prometheus retention stays at 30d;
  no long-term store. (The `gawk-admin` activity log in Postgres already
  keeps lifecycle events for longer if that question ever comes up.)
- **OD6 — Coarse client labels are acceptable.** A fixed vocabulary of app,
  OS, browser, codec and resolution tier. No user agent, IP or location,
  ever.
- **OD7 — Quality appears as a daily trend only**, from the existing R9
  counters. No per-broadcast drill-down.

---

## 3. Design decisions

### D1 — The client identifies itself with query parameters, not a wire type

The publish and subscribe dials gain three optional parameters:

| Param | Values | Sent by |
|---|---|---|
| `app` | `web`, `desktop` | web app; desktop broadcaster |
| `os` | `windows`, `macos`, `linux`, `android`, `ios`, `chromeos` | both |
| `browser` | `chromium`, `firefox`, `safari` | web app only |

The relay maps each to a closed vocabulary before it becomes a label: absent
is `unknown`, anything outside the list is `other`. So the label set is
bounded whatever a client sends, and a new client value costs a relay
release, not a cardinality problem. The raw values are never logged.

A wire message was the alternative. It would cost a type in all four
mirrors (CLAUDE.md), for data that is only ever a metric label, and the
dial URL already carries `secret`, `resume`, `delivery`, `parity` and
`owner`. A relay that predates R59 ignores the parameters, which is the
degradation we want.

The frozen Go broadcaster (`gawk-broadcast`, fix-only since R56) sends
nothing and shows as `unknown` until R56 retires it.

### D2 — Codec and resolution come from the media

The broadcaster dials before capture starts (`broadcaster.ts` connects, then
`startMedia()`), and the R4 ladder changes resolution mid-session, so a
dial-time `res` parameter would be wrong for most of a broadcast. The relay
already sees the truth:

- **Codec** from the `DecoderConfig` codec string: `avc1`/`avc3` → `h264`,
  `vp09` → `vp9`, `vp8` → `vp8`, `av01` → `av1`, else `other`.
- **Resolution** from each cached keyframe: the H.264 SPS (from the AVCC
  extradata in the config, or in-band in an Annex B keyframe), the VP8
  keyframe header, or the VP9 uncompressed header. Anything unparseable is
  `unknown`.
- **Tier** by the coded height: `≤480` → `sd`, `≤720` → `720p`, `≤1080` →
  `1080p`, `≤1440` → `1440p`, larger → `2160p`.

Parsing is header-only and happens once per keyframe, which is at most two a
second. A parse failure never touches the media path.

### D3 — "New" is the mint path; everything else is "resumed"

`gawk_broadcasts_started_total{kind}` counts an accepted publish session:
`kind="new"` on the mint path (`/publish`), `kind="resumed"` on the claim
path (`/publish/{id}`). The claim path covers a broadcaster's reconnect, a
re-home to another pod and a reclaim after a relay restart; none of those is
a new broadcast. Broadcasts per day is `increase(…{kind="new"}[1d])`.

### D4 — A viewer join is a primary session, and the app marks its rejoins

`gawk_viewer_joins_total{kind}` counts accepted `/subscribe` sessions that
are not R30 stripe legs (`leg` param) and not edge pulls
(`/internal/subscribe`). The web app's `ViewerSession` builds a new pipeline
per reconnect attempt; every pipeline after the first sends `rejoin=1`, so
`kind` is `first` or `rejoin`. Viewers per day is
`increase(…{kind="first"}[1d])`. A browser reload is a new `first`, which
matches what a person does.

### D5 — Durations and peaks are observed when things end

- `gawk_viewer_session_seconds{delivery}`: a histogram observed when a
  counted join's session closes. A rejoin starts a new observation, so a
  reconnect splits one watch into two; the `rejoin` counter shows how often.
- `gawk_broadcast_duration_seconds` and `gawk_broadcast_peak_viewers`:
  observed when an **origin** hub is removed (grace expiry, end, or
  operator termination). Duration runs from the hub's creation to its
  publisher's last disconnect, so the grace period isn't counted. The peak
  is the largest global viewer count the origin computed (local viewers plus
  edge reports; stripe legs excluded).

Two known gaps, accepted: a relay restart loses the observation for the
broadcasts it held, and a re-home splits one broadcast into two
observations on two pods. Both are rare and both undercount; the dashboard
labels the panels as such.

Buckets: durations `60, 300, 900, 1800, 3600, 7200, 14400, 28800` seconds;
peaks `0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000`.

### D6 — The client mix is a per-broadcast info gauge

`gawk_broadcast_info{broadcast, codec, resolution, app, os, browser} 1` on
the origin pod for each live broadcast. `broadcast` is the same HMAC'd key
as every other per-broadcast series (CLAUDE.md: never a raw ID). "Share of
broadcasts on H.264" is `count by (codec) (gawk_broadcast_info)`, and it can
be weighted by viewers with a join on `broadcast`. The viewer side has no
per-session gauge; `gawk_viewer_joins_total` carries `app`, `os` and
`browser` (and `delivery`).

### D7 — The caps are exported as `gawk_limit{name}`

One const gauge per configured cap, from the same `config.Config` the
registry is built from: `max_broadcasts`, `max_subscribers` (per broadcast),
`max_total_subscribers`, `max_bandwidth_bytes` (bytes per second), `dvr_max_bytes`, `max_rooms`,
`max_room_broadcasts`, `max_room_participants`, `conn_rate_limit`,
`conn_burst_limit`. `0` means unlimited, as in the flags. No new knob, so
nothing new to plumb through `registryOptions`.

### D8 — Dynamic room mints are counted

`gawk_rooms_minted_total` counts dynamic rooms this pod mints, alongside the
existing `gawk_rooms_live{kind}` gauge. Static rooms are declared as Room
CRs, not opened by anyone, so they have no creation event worth counting.

### D9 — `gawk-admin` gets its own metrics listener

Same pattern as the relay (docs/13 D1) and `gawk-telemetry`: a third plain
HTTP listener, `-metrics-addr` / `GAWK_ADMIN_METRICS_ADDR`, default `:8091`,
`off` disables. The chart exposes it on a ClusterIP Service with an optional
ServiceMonitor (`metrics.serviceMonitor.enabled`, default false) and never
adds it to the Ingress. It serves:

- `gawk_admin_build_info{version}`, and the Go and process collectors;
- `gawk_admin_http_requests_total{route, code}`, where `route` is the
  `net/http` pattern that matched (bounded) and `code` is the status class
  (`2xx` … `5xx`);
- `gawk_admin_db_pool_*` from `pgxpool.Stat()` (acquired, idle, total, max,
  acquire count and wait time);
- `gawk_admin_events_ingested_total{outcome}` from the event-bus consumer;
- `gawk_admin_webhook_deliveries_total{outcome}` from the dispatcher.

### D10 — The rest is deployment configuration

Enabling the `gawk-telemetry` ServiceMonitor and CNPG's PodMonitor
(`spec.monitoring.enablePodMonitor`) are values in the deployment's GitOps
repository, not code. UM5 covers them and the self-hosting doc.

---

## 4. Metric catalogue (new in R59)

| Metric | Type | Labels | Where |
|---|---|---|---|
| `gawk_broadcasts_started_total` | counter | `kind`, `app`, `os`, `browser` | relay |
| `gawk_viewer_joins_total` | counter | `kind`, `delivery`, `app`, `os`, `browser` | relay |
| `gawk_viewer_session_seconds` | histogram | `delivery` | relay |
| `gawk_broadcast_duration_seconds` | histogram | — | relay (origin) |
| `gawk_broadcast_peak_viewers` | histogram | — | relay (origin) |
| `gawk_broadcast_info` | gauge | `broadcast`, `codec`, `resolution`, `app`, `os`, `browser` | relay (origin) |
| `gawk_limit` | gauge | `name` | relay |
| `gawk_rooms_minted_total` | counter | — | relay |
| `gawk_admin_build_info` | gauge | `version` | admin |
| `gawk_admin_http_requests_total` | counter | `route`, `code` | admin |
| `gawk_admin_db_pool_*` | gauge/counter | — | admin |
| `gawk_admin_events_ingested_total` | counter | `outcome` | admin |
| `gawk_admin_webhook_deliveries_total` | counter | `outcome` | admin |

`delivery` is `datagrams`, `reliable` or `dvr`, as negotiated.

---

## 5. Chunks

| Chunk | Scope | Acceptance criteria |
|---|---|---|
| UM1 | **Relay**: D1 label normalisation; D2 media probe (codec, SPS/VP8/VP9 resolution); D3–D5 counters and histograms in the transport handlers and at origin hub removal; D6 info gauge in the registry collector; D7 `gawk_limit`; D8 rooms minted | Tests named in §1; the collector tests assert exact label sets; `go test -race ./...` green; `/statusz` unchanged |
| UM2 | **Web app**: `app`/`os`/`browser` on the publish and subscribe dials; `rejoin=1` on every `ViewerSession` pipeline after the first | `client-identity.test.ts` maps real user agents (the engine on iOS is WebKit whatever the browser); the publish and subscribe URL tests see `app=web`; `viewer.test.ts` sends `rejoin=1` only when asked and `viewer-session.test.ts` asks on every pipeline after the first; vitest, lint and `tsc -b` green |
| UM3 | **Desktop broadcaster**: `app=desktop` and `os` from the build target on the publish dial | `relay.rs` URL test per target; `cargo test` green |
| UM4 | **`gawk-admin`**: D9 listener, collectors and instrumentation; flag, env and chart values (`metrics.enabled`, `port`, `serviceMonitor.*`) | Unit tests: the listener serves `/metrics`; `off` starts none; the route label is the pattern, not the path; `helm template` renders the Service and, only when enabled, the ServiceMonitor, and the Ingress never gets the port |
| UM5 | **Deployment**: `gawk-telemetry` ServiceMonitor, `gawk-admin` ServiceMonitor and CNPG PodMonitor in the fleet's GitOps values; `docs/self-hosting.md` lists what to enable | Prometheus shows `up == 1` for all three targets |
| UM6 | **Dashboard**: the usage rows switch to the new series (exact broadcasts and joins, durations, peaks, client mix, caps from `gawk_limit`) and gain `gawk-admin` and database health | Every new panel returns data in production after UM1–UM5 deploy |

UM1 comes first; UM2–UM4 are independent of each other; UM5 needs UM4
released; UM6 needs everything deployed.

---

## 6. Verification on the reference deployment (2026-09-30)

**UM5**: Prometheus shows `up == 1` for `gawk-admin-metrics` (both pods),
`gawk-telemetry-metrics` and the CNPG exporter of `postgres-gawk-admin`
(both instances).

**UM6**: the `gawk.ioio.fi` dashboard (version 4) now reads the new series:

- The summary stats are *New broadcasts* (`gawk_broadcasts_started_total{kind="new"}`)
  and *Viewer joins* (`gawk_viewer_joins_total{kind="first"}`). The daily bars
  split broadcasts into new/resumed and joins into first/rejoin.
- A new *Sessions & clients* row shows broadcast length, viewer watch time
  and peak viewers as bucket distributions over the selected range; live
  broadcasts by codec/resolution and by broadcaster client; and joins by
  delivery and by client.
- The cap lines on the capacity panels (viewers per pod, broadcasts per pod,
  busiest broadcast, DVR ring, rooms) come from `gawk_limit`, and a
  *Configured limits* table lists every cap. No panel hardcodes a cap any more.
- A new *gawk-admin & database* row shows portal requests and failed probes;
  events ingested and webhook deliveries; the pgx pool; and CNPG exporter
  health, database size, replication lag, backends and commits.

Every new query returned data against production at the current time. Two
things are still expected:

- The per-day and per-hour bars fill in from the first full window after
  the deploy.
- `gawk_admin_webhook_deliveries_total` has no series until a webhook
  delivery happens.

A caveat for reading the counters: `increase()` cannot see a labelled
counter's first increment, because the series is born at 1. Each label
combination therefore undercounts by one the first time a relay pod sees
it (and again after each pod restart). Rare client combinations are the
ones this visibly affects.
