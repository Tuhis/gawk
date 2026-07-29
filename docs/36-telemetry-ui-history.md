# R31 — Telemetry UI v2: a purpose-built diagnosis SPA

**Status**: requirements drafted 2026-07-29, owner decisions taken the same day
(§3.2), not started. Chunks **TH1–TH11** (two-letter prefix; `A`–`Z`, `CG`,
`DV`, `FP`, `LI`, `MF`, `NA`, `QL`, `ST`, `TM` are claimed).

**Scope**: the `gawk-telemetry` read surface — `gawk-telemetry/ui/` plus the
read API that feeds it, plus one chart-default change and one new small write
path. **Zero wire, relay, viewer and broadcaster changes**, and per R28 §7
**no new measurement**: every number named here is already collected, already
stored, and in most cases already served.

---

## 0. Orientation — start here

### 0.1 Reading order

1. [`docs/33`](33-telemetry-and-diagnostics.md) **§4.6** (read API) and **§4.8**
   (the dashboard) — what exists and *why it is shaped that way*. §8 names the
   standing risks this item inherits.
2. [`CODE-REVIEW.md`](../CODE-REVIEW.md) — **bug fixes are test-first, always.**
   Two of this item's chunks (TH1's dead permalink, TH2's deleted history
   layer) are fixes, not features, and the failing test comes first.
3. [`gawk-telemetry/README.md`](../gawk-telemetry/README.md) — the dev loop.
   **Do not iterate on the UI through commit → PR → release → deploy**; the
   README's recipe points a Vite dev server at a *real* deployed backend
   through a port-forward, with the basic-auth header injected by the proxy.
   Vite binds IPv6 first, so use `http://localhost:5174` — `127.0.0.1` is
   refused.
4. This document.

### 0.2 A numbering trap

**This document has its own D1–D22, and docs/33 has its own D1–D17. They
collide.** Every reference to an R28 decision here is written
**`R28 D<n>`**; a bare `D<n>` always means *this* document. When in doubt,
check which list the sentence is arguing from — e.g. `R28 D10` is the 32 KB
context ceiling, while this document's D10 is "absence is never green".

### 0.3 The UI as it stands, and its fate

`gawk-telemetry/ui/src/` — ~1 270 lines of TypeScript excluding tests. What
each file is and what R31 does to it:

| File | Today | Under R31 |
|---|---|---|
| `App.tsx` | The whole page: header, live group, ended group | Becomes the Live *section* behind TH1's router |
| `api/client.ts` | `fetchLive`, `resolveCode`, `probeResolve`. **All paths relative** — deliberate, so it works on `/`, on a port-forward and under an Ingress sub-path | Grows the historical endpoints; keep the relative-path rule |
| `api/types.ts` | Hand-written mirror of the Go types. Its header states the rule that matters: **absent and zero are different claims** | Extends; the rule is load-bearing for every new chart |
| `components/TimelineChart.tsx` | 228 lines of hand-rolled SVG | **Replaced** by the ECharts wrapper (D11) — but its two behaviours must survive: gaps drawn as breaks (D9) and a fixed viewBox so nothing reflows as values tick |
| `components/SessionTimeline.tsx` | Two hard-coded chart pairs, per role | Superseded by TH2 + TH5 |
| `components/SessionTable.tsx` | The dense row table | The model for TH3's list; gains virtualization (D12) |
| `components/BroadcastCard.tsx` | Live card, renders `finding.verdict` only | Gains TH6's evidence drill-down |
| `components/FindStream.tsx` | Code → obfuscated key | Extends to scope TH3 |
| `components/SeverityBadge.tsx`, `lib/severity.ts`, `lib/format.ts` | Severity model + formatters | Keep; `format.ts` gains absolute-time helpers (D5) |
| `lib/history.ts` | Client-side timeline accumulation | **Deleted** — see §1.1 |
| `state/liveStore.ts` | The 2 s poll loop | Becomes SSE-fed with the poll as fallback (D22) |
| `state/uiStore.ts` | Card / timeline expansion state | Largely subsumed by URL state (TH1) |

### 0.4 Gates, and two traps in them

```sh
cd gawk-telemetry/ui && npm ci && npm run lint && npm test && npm run build
cd gawk-telemetry   && go build ./... && go vet ./... && go test -race ./...
```

- **`npm run build` is the only real typecheck.** The root tsconfig is
  solution-style, so a bare `tsc --noEmit` passes *vacuously*, and vitest
  strips types rather than checking them. A change that only ran `npm test` has
  not been typechecked.
- **The no-external-fetch tests skip unless the bundle is built.**
  `TestNoExternalAssetReferences` / `TestServesTheEmbeddedPage` live in
  `internal/dashboard/` but assert against `dist/`, which the Go job never
  builds — so in CI they run in the **`telemetry-ui`** job, after `npm run
  build`, and *skip by design* in the `telemetry` job. Running
  `go test ./internal/dashboard/` locally without building the UI first will
  show green while proving nothing. **This is the test that guards D11's
  bundling decision**, so it is the one to run deliberately after adding
  ECharts.
- `go build` never depends on npm: `dist/` carries one committed `README.md` so
  `//go:embed dist` always compiles, and a binary built without the bundle
  serves a short "UI not built" page with its API and MCP unaffected. Asset
  filenames are **stable, not content-hashed**, because the page is served
  `no-store`.

### 0.5 Chunk dependencies

TH1 blocks everything (it is the router and the URL state every other view
stores itself in). Otherwise: TH9 needs TH4's timeline and TH6's verdict
surface; TH5's field catalogue improves TH2 and TH4 but does not block them;
TH11's criteria bind each surface **as it ships**, rather than landing at the
end. TH2 reads the same stored timeline TH5 plots, so build the fetch layer
once.

---

## 1. Why now

R28 built a store and a query surface, and then wired a **live-only** page to
it. The asymmetry is stark enough to be the whole justification:

| Read endpoint | Serves | Consumed by the UI |
|---|---|---|
| `GET /live` | live fleet projection | **yes** (2 s poll) |
| `POST /v1/resolve` | code → obfuscated key | **yes** |
| `GET /v1/broadcasts` | broadcasts since T, worst-first | no |
| `GET /v1/sessions` | per-session rollup summaries | no |
| `GET /v1/sessions/{id}` | stored timeline + **every event** | no |
| `GET /v1/sessions/{id}/diagnose` | ranked verdicts + evidence | no |
| `GET /v1/sessions/{id}/compare` | session vs fleet median | no |
| `GET /v1/fleet` | grouped percentiles | no |

Two of eight. Everything historical the service knows is reachable only by
`curl` or by an operator with Claude Code pointed at MCP. The dashboard is
excellent for the ten seconds after someone says "it's stuttering" and has
nothing to say ten minutes later.

Three gaps are not missing features but **active defects**:

1. **`diagnose()` emits dead links.** `readapi.go` sets
   `rep.DashboardURL = base + "#/session/" + sessionID`. The SPA has no router;
   every such URL lands on the fleet page. Those URLs are serialized into the
   `verdict` blob of every rollup row, and **rollups are permanent** — so the
   defect is being written into the one artifact that is never pruned.
2. **The timeline is a tab-local memory, not data.** `lib/history.ts`
   accumulates from the polls the page already makes: 10 minutes, 600 points,
   dropped 5 minutes after a session disappears, gone on reload. The graph is
   at its best *during* the incident and worthless *after* it — exactly
   inverted from what a post-incident review needs.
3. **The dashboard asserts where the API argues.** `BroadcastCard` renders
   `finding.verdict` as a bare sentence. `Evidence`, `Confidence`, `Action`,
   `Report.Passed` and `Report.Unavailable` — **R28 D6/D7**'s entire provenance
   apparatus, the thing that makes a verdict inspectable rather than believed —
   never reach a screen.

The timing is also forced: **TM9 (Grafana) was dropped** by owner scope
decision, and the owner has now deferred it again (2026-07-29). This SPA is
the only planned home for a trend question. "Did R29's parity actually cut
frame loss on the fleet?" is today a hand-written DuckDB query.

### 1.1 The finding that shapes the design

`store.ReadSession` opens with:

> Flush anything buffered for this session so a read during a live session
> sees what has been appended.

**The full-resolution timeline of a session happening right now is already on
disk and already served.** The client-side accumulation in `lib/history.ts` —
with its "history starts when the page is opened" caveat — was never
necessary. It is not a shortcut to extend; it is a layer to delete.

This dissolves the largest requirement here at a cost of roughly zero: no new
storage, no in-RAM ring, no new measurement. It also sets the boundary
precisely. `internal/live`'s stance — *"Disk is for history; the live page
never reads a session file"* — is correct and stays: it governs the **fleet
scan**, which touches every session every 2 seconds. Reading one file because
a human opened one session is a different cost profile by three orders of
magnitude, and conflating the two would give up the best result available.

---

## 2. What an SRE has to answer, and what fails today

Requirements are derived from these, not from a feature wishlist. The loop is
the ordinary one — **detect → scope → isolate → confirm → verify** — and this
system's failure modes make isolation the expensive step.

| # | The question | Today |
|---|---|---|
| Q1 | *Is anything wrong right now?* | **Answered well.** TM8 is good and stays the landing view. |
| Q2 | *It was bad at 21:04. Show me.* | Unanswerable in a browser. Nothing before the tab opened exists. |
| Q3 | *Show me this whole stream on one axis — broadcaster and all five viewers.* | Impossible: each session graphs alone. |
| Q4 | *Did they all dip at the same second, or just that one viewer?* | The isolating question, structurally unanswerable. |
| Q5 | *Did the whole fleet have a bad minute at 21:04?* | No cross-broadcast view of any kind. |
| Q6 | *Plot `parityInsufficient` / `avSkewMs` / `stripeLargeLossPct`.* | ~13 fields are plottable; ~80 exist. |
| Q7 | *Why do you say it's bad? What is that resting on?* | Verdict sentence only; evidence and provenance dropped. |
| Q8 | *Is this bad, or is this normal here?* | `compare`/`fleet_summary` exist, unused by the UI. |
| Q9 | *Did the fix work? Is the fleet better on 1.4.0 than 1.3.x?* | DuckDB by hand — and no engine is actually wired (§3.2 D18). |
| Q10 | *Send me the link / put it in the doc / note what I found.* | No URL state, no export, no annotations. |
| Q11 | *Stop moving while I read you.* | 2 s poll, no pause, no freeze. |
| Q12 | *What am I **not** seeing?* | Retention boundary, relay coverage gaps and downsampling are invisible. |

---

## 3. Decisions

### 3.1 Design stances

**D1 — The 32 KB ceiling is a machine-surface rule, not a UI budget.**
**R28 D10** exists to stop 80 fields × 200 samples from incinerating a model's
context. A 2 000-point chart is an illegitimate MCP *default* and an ordinary
browser request. **Every existing default stays byte-identical**; the UI opts
in explicitly (`fields`, `points`, and the new `from`/`to`), and new
UI-shaped endpoints declare their own bounds. The existing test asserting the
ceiling against a synthetic 4-hour session must keep passing untouched — if it
needs changing, the change is wrong.

**D2 — Render the stored verdict; never recompute from a rollup.**
`severityOfRow` already reads the stored verdict, because it was computed when
the raw window still existed and by now that window may be pruned. Historical
views inherit this, and must display *when* a verdict was computed and against
what `relayCoverage`: a `bad` resting on client testimony alone is a different
claim from one a relay counter corroborated, and **R28 D7** says so in the data
already.

**D3 — The UI is a reader of measurements.** R28 §7 stands: no new
measurement, no new client-side collection. (Annotations, D16, are operator
prose — not a measurement.)

**D4 — Filtering, sorting, bucketing and pagination are server-side.**
Shipping 14 days of rollups to a browser to filter them there is the same
category error as returning 80 fields to a model.

**D5 — Absolute time is the primary axis for anything historical.**
Today the vocabulary is `ago()`/`dur()` with one `clockTime` in the header —
right for the live page, where "now" is the anchor; wrong everywhere else.
Correlating with relay logs, Prometheus, a release timestamp or a friend's "it
broke around nine" needs a wall clock. Relative time becomes the annotation.
The timezone is displayed, never assumed.

**D6 — Deep links are a correctness fix, not a feature.** `#/session/<id>`
must resolve, because it is already stored in permanent artifacts. TH1 ships
before anything that emits more of them.

**D7 — Exposure posture unchanged.** The read listener is never routed
publicly (R28 D14); no new public surface; **no external asset fetch**, so the
page keeps working on a port-forward from a laptop with no network — asserted
by an existing test over the built output, which must keep passing. A chart
library may be *bundled*; none may be *fetched*, and no web font may be
loaded from a network.

**D8 — Counters and gauges are different things and the UI must know which.**
Plotting a cumulative counter as a line is near-useless; what an operator
wants is its rate. The service types every known field
(`schema.ViewerFields`) but exposes no kind, unit or semantics. The UI must
not fork that list — **R28 D15** exists precisely because a second copy of a
field list drifts. Hence a server-owned **field catalogue endpoint** (TH5).

**D9 — Gaps stay breaks; envelopes stay envelopes.** `TimelineChart` already
draws each contiguous run as its own subpath, because a metric that stopped
being reported did not glide to its next value. Every new chart inherits that.
Likewise `get_session`'s downsampling is envelope-preserving (worst-of-bucket,
a real sample, never interpolated) — so the UI must **say** it is showing
worst-of-N, or a reader takes the line for raw.

**D10 — Absence is never green, and now never blank either.** The live model's
rule extends to history: a session whose raw window was pruned reads *"raw
expired — rollup only"*, not an empty chart. A broadcast with no relay
coverage says so on the axis. The failure this prevents is concluding "nothing
was wrong" from "nothing was kept".

### 3.2 Decisions taken with the owner (2026-07-29)

**D11 — Charting is Apache ECharts, bundled.** Chosen over visx, uPlot and
Recharts/Nivo. The load-bearing reason is not looks: **`echarts.connect()`
synchronises crosshair and zoom across separate chart instances**, which *is*
TH4's multi-lane broadcast timeline and TH7's fleet timeline — built in rather
than built by us. Canvas rendering makes 5 lanes × 2 000 points a non-event;
`markArea`/`markLine` give hidden-tab shading, dip episodes and event markers
natively; `dataZoom` and `brush` give range selection.

Two implementation constraints follow. **Import from `echarts/core` with
explicit component registration**, not the barrel — tree-shaking is ours to
control and the whole-package import is several times the size. And **wrap it
in a small local React hook**, not `echarts-for-react`: one dependency instead
of two, and lifecycle/dispose behaviour we own. D7's no-external-fetch test
(`internal/dashboard/dashboard_test.go`) is what proves the bundle is genuinely
self-contained.

**D12 — Scale target: ~50 broadcasts / ~200 viewers.** Lists are virtualized
(TanStack Virtual — headless, tiny), aggregation stays server-side where it
already is, and a 1 000-viewer broadcast is an acknowledged follow-up rather
than a v2 requirement. This is deliberately above the homelab's reality and
below R17's ceiling.

**D13 — The live fleet page stays the landing view.** TM8's surface is what
you open when someone says "it's stuttering", and it does that job. History,
Explore, Fleet and Rules become peer sections behind a nav, reachable in one
click. Nothing about the existing scan surface moves.

**D14 — Dense ops console, dark by default.** ~12px type, a sparkline on every
row, several lanes visible without scrolling — the idiom that fits the job of
scanning for the anomaly. Deliberately *not* gawk-app's R6 monochrome tokens:
those were designed for a cinematic viewer, and a data console is a different
problem. Hue stays reserved for severity; density carries everything else. The
README's existing no-layout-shift rules (`table-layout: fixed`, percentage
colgroup, `tabular-nums`, reserved slots) become harder, not softer, at this
density and bind every new surface.

**D15 — Raw retention becomes configurable with a default of 30 days.**
`-retention-days` already exists; this is a chart default change plus the
trade-off documented in the chart README. Thirty days covers a full release
cycle, so "compare this session to one from before the R30 change" stays
answerable at full resolution instead of rollup-only. ~160 MB at current
volume. Rollups stay permanent regardless.

**D16 — Annotations are the one write path.** A note pinned to a session or to
a moment on a timeline ("switched to WiFi here", "this is the R30
regression"). Stored beside rollups, **permanent** — an annotation outliving
the samples it describes is the normal case and the point — never mixed into
session files, so a raw partition stays exactly what a client sent. They
export into the markdown a finding already produces, which fits how this
project records field findings into `docs/*.md`. Free-text authored by the
operator, on the operator's own PVC: R28's zero-PII rule governs *collected*
data and is untouched.

**D17 — Mobile is read-only triage.** The live list, severity, verdicts and a
single-metric chart work on a phone. The multi-lane timeline, the metric
explorer and the SQL console are desktop-only **and say so** rather than
degrading into something unreadable. Covers "someone messages me while I'm on
the couch" without paying for dense synchronised time series on a 390px
viewport.

**D18 — A SQL console, on by default — which requires actually building the
engine.** The owner's decision, recorded with the constraint attached because
it is not a flag flip:

- `internal/mcp` accepts `EnableSQL`, but `main.go` **never wires a `SQL`
  func**. Enabling it today produces a tool that answers *"query_sql is
  enabled but no engine is wired in this deployment"*. The feature is a stub.
- `gawk-telemetry/go.mod` has **zero third-party dependencies**, and the image
  builds `CGO_ENABLED=0` into `distroless/static-debian12`. Every usable Go
  DuckDB driver is cgo.

So "on by default" costs: a cgo dependency, a base image that is no longer
`static`, a larger image, and the end of the module's cgo-free property.
It also reverses R28 D11's posture that arbitrary SQL should be a deliberate
act. **Recommended resolution, pending the owner's call (§8 Q1)**: build the
console and wire the engine, keep the *flag* as the gate but flip its default
to **on**, and ship the cgo variant as the deployed image while `go build` on
a fresh clone stays cgo-free with the tool reporting itself unavailable. That
delivers the decision — the console is there by default on the deployment —
without making a laptop `go build` depend on a C toolchain. The console's
results feed the chart components, so an ad-hoc query is plottable rather than
a table of numbers.

**D19 — In-tab watch, and nothing more.** Star a live broadcast: it pins to
the top and visibly changes when its severity escalates. No browser
notifications (Chrome throttles a background tab to ~1/min, so an alert could
arrive a minute late — worse than useless for a stuttering stream), no sound,
no server-side state, no rules. R28's non-goal was alerting *infrastructure*;
a page noticing something while you look at it is not that.

**D20 — Full read-only rule transparency.** A rule catalogue (every playbook
rule, its thresholds, required signals, provenance) plus a per-session trace:
for each rule, what it read, what it compared against, and why it did or did
not fire. **No tuning from the UI** — stored verdicts were computed under the
thresholds of their day, so editable thresholds would make history and live
disagree unless every verdict recorded the config it ran under. This directly
mitigates the standing risk docs/33 §8 names: one engine wrong in two places
at once.

**D21 — Dip episodes are explained, including across sessions.** Click a dip
and see every metric that moved materially inside its window, ranked by
magnitude, relay and client side together — TM10 already captures counter
deltas per episode, so this is mostly a projection of existing data. Plus:
automatically check whether *other viewers on the same broadcast* dipped in
the same window and say so, which mechanises the isolating question TH4 makes
visible. The correlation **must state its own confidence** — with one
reporting viewer there is no correlation to draw, and saying "only one viewer
reported" is the honest output, not a coincidence.

**D22 — Live data moves to SSE, with polling as the fallback** *(my call, not
the owner's — flagged for objection)*. At D12's scale the `/live` payload
carries findings and metrics for every session every 2 s, and TH8's watch plus
several open views multiply it. An `EventSource` on the read listener pushes
changes and degrades to the current 2 s poll when the stream drops. It works
through a port-forward and behind the internal Ingress, which is the whole
deployment surface. The projection is unchanged; only its delivery is.

---

## 4. Chunks

Three waves. **TH1 is a correctness fix and unblocks the rest; TH2 and TH4
carry the item's value** (Q2 and Q4). TH11's criteria bind every surface as it
ships rather than landing at the end.

### Wave 1 — foundations and history

#### TH1 — Routing, permalinks and URL state

A hash router (matching `gawk-app`'s convention, which the dashboard's SPA
fallback already assumes). Every view addressable; view state — time range,
filters, selected sessions, chosen metrics, expansion — lives in the URL, not
in a store the address bar knows nothing about.

Routes: `#/` (live fleet) · `#/broadcast/<key>` · `#/session/<id>` ·
`#/history` · `#/explore` · `#/fleet` · `#/rules` · `#/sql`.

| Acceptance criterion | Verified by |
|---|---|
| `#/session/<24-hex>` resolves for a session that is live, ended, or ended weeks ago | test + manual |
| A `DashboardURL` taken verbatim from a stored rollup `verdict` opens the session it names | test against a stored row |
| Copying the address bar and reopening reproduces range, filters and selections exactly | manual |
| Back/forward move between views without a full reload and without losing poll state | manual |
| An unknown or malformed id renders "no such session", never a blank page or an endless spinner | test |
| A pruned session id renders the rollup-only view (D10), not an error | test |

#### TH2 — Session detail

The page the dead link has been pointing at. One session, everything known
about it, **from disk** — full resolution, from its first sample, for a live
session as well as an ended one (§1.1).

Identity and config header (role, browser/OS, `appVersion`, delivery mode,
renderer, pipeline context, acceleration, codec, target) · the verdict with
full evidence (TH6) · charts over the curated set with **events as
`markLine`s** (`Timeline.Events` is already returned and nothing renders it:
close codes, reconnects, config applied, resync) · `documentHidden` as a
`markArea`, as the live view already shades · a data-quality banner from the
existing `distrustReason()` (`truncated`, `seqGaps`, `schemaAnomalies`) ·
`relayCoverage` banded on the axis · `compare` against the fleet median for
its class.

`lib/history.ts` and its "history starts when the page is opened" caveat are
**deleted**, not extended.

| Acceptance criterion | Verified by |
|---|---|
| A session that started 40 minutes ago shows all 40 minutes, in a tab opened one second ago | test + manual |
| A live session updates as batches land, without re-fetching the whole file each tick | test on the windowed fetch |
| Every stored event appears as a labelled marker at its own timestamp | test |
| A truncated / gap-carrying / anomaly-heavy session shows its `distrust` sentence above its charts | test |
| `relayCoverage: "none"` states that the verdict rests on client testimony alone | test |
| The fleet page's per-poll cost is unchanged — no session file is read by `/live` | test asserting the fleet path opens no session file |

#### TH3 — History browser

`#/history`: broadcasts and sessions over a chosen range, server-side filtered,
sorted and paginated (D4), virtualized (D12). Filters: range, role, severity,
broadcast key, browser/OS, delivery mode, `appVersion`, has-findings,
distrusted. Sorts: severity, start, duration, stalls. Columns are the existing
`SessionSummary` projection — built for exactly this and unused.

The R28 **Find** box (code → obfuscated key, `POST /v1/resolve`) extends to
scope the history browser, not merely to highlight a live row.

| Acceptance criterion | Verified by |
|---|---|
| A session that ended yesterday is reachable in ≤ 2 interactions from a cold page | manual |
| Filters and range are in the URL and survive a reload (TH1) | test |
| Filtering and sorting happen server-side; the browser never holds the unfiltered set | test on request shape |
| 2 000 rows scroll at 60 fps and hold bounded memory | measured |
| The raw-retention boundary is drawn: rows past it are marked rollup-only (D10) | test |
| A range with no data reads differently from a range the service cannot answer | test |

### Wave 2 — correlation and depth

#### TH4 — Broadcast timeline

**The isolating surface; the answer to Q3/Q4.** One broadcast, one absolute
time axis, one lane per participant: broadcaster on top, each viewer below,
plus a relay lane. Lanes are ECharts instances joined by `echarts.connect()`,
so one crosshair and one zoom govern all of them (D11).

Each lane carries its session's span (join → leave), severity as a band over
time, its primary rate, and its events as markers. The relay lane carries the
scraped observations for that broadcast — ingress loss, `framesRelayed` rate,
egress drops, pod and origin/edge role, **including re-homes**, which R17
makes routine and no current view can show.

Sources have different clocks and cadences — client batches ~10 s, relay
scrape ~5 s, live projection 2 s — so each lane is drawn at its own resolution
and **never implies sub-cadence precision**; the cadence is stated per lane.
Every record carries both `tMs` (client) and `receivedAtMs` (service), which
is what makes placing a skewed client on a shared axis legitimate.

| Acceptance criterion | Verified by |
|---|---|
| A broadcast with a broadcaster and ≥ 3 viewers renders all lanes on one aligned axis | test |
| One crosshair reports every lane's value at that instant; one zoom moves all lanes | manual |
| A fixture where all viewers dip together and one where a single viewer dips are distinguishable at a glance | test fixture + manual |
| Joins and leaves appear as span boundaries, not as gaps in a line | test |
| A relay re-home (pod/role change mid-broadcast) is marked on the relay lane | test |
| Each lane states its own cadence; no lane is drawn at a resolution its source cannot support | test |
| 5 lanes × 2 000 points render in < 250 ms | measured |

#### TH5 — Metric explorer

`#/explore`: any recorded field, over time, for one or more sessions.

Requires the **field catalogue** (D8) — `GET /v1/fields` — returning per field:
name, applicable role(s), kind (`gauge` | `counter` | `bool` | `text`), unit,
one-line description, and whether it is in the curated set. Server-owned,
derived from `schema.ViewerFields`/`BroadcasterFields`; the UI never carries a
second list.

Behaviour follows from the catalogue: a **counter is offered as a rate** by
default with the cumulative form one click away; a **bool renders as a band**,
not a line between 0 and 1; units drive axis labels and formatting; unknown
fields (D15 keeps them verbatim) appear in a labelled "unknown type" group
rather than vanishing. Plus multiple series with independent axes where units
differ, **the same metric across several sessions on one axis** (the A/B move
— the viewer that is fine beside the one that is not), and full resolution on
request with D9's disclosure when it is not.

| Acceptance criterion | Verified by |
|---|---|
| Every field in `schema.ViewerFields`/`BroadcasterFields` is selectable and plottable | test over the catalogue |
| Adding a field to the Go tables makes it appear in the picker with no UI change | test |
| A counter defaults to a rate; the cumulative form is available and labelled | test |
| A downsampled chart says "worst of every N samples" (D9) | test |
| The same metric for 2+ sessions renders on one aligned axis | test |
| A field with no data in range renders as absent, never as zero | test |

#### TH6 — Verdicts, evidence and the rule catalogue

Findings render as arguments. Per finding: severity, verdict, **each evidence
row with value, unit, comparison and a provenance chip** (`relay` / `client` /
`derived`), confidence with its cap explained where **R28 D7** capped it, and
the playbook `action`. Per report: `Passed` rules (so healthy is distinguishable
from never-analysed), `Unavailable` with the signal each rule was missing, and
every caveat.

Plus D20's transparency: `#/rules` lists every rule with thresholds, required
signals and provenance; a per-session trace shows what each rule read and why
it did or did not fire. Read-only. Evidence signals link into TH5 with that
field plotted over the session's range — the shortest path from *why do you
say that* to *let me look*.

| Acceptance criterion | Verified by |
|---|---|
| Every field of `rules.Finding` and `rules.Report` is rendered somewhere; none dropped | test enumerating the structs |
| A client-only verdict visibly reads as weaker than a relay-corroborated one | manual |
| A healthy session shows its `passed` checks, not an empty panel | test |
| `unavailable` rules name the missing signal | test |
| The rule catalogue's thresholds come from the Go rules, never a second copy | test |
| A trace explains a non-firing rule in terms of the numbers it read | test |
| A finding is copyable as markdown, evidence included, for pasting into `docs/*` | test |

### Wave 3 — fleet, intelligence, escape hatches

#### TH7 — Fleet timeline and trends

`#/fleet`, first-class (owner decision). Two halves.

**The fleet timeline**: one row per broadcast over a chosen range, severity as
a band, all rows on one shared axis — so a relay-wide or pod-wide event shows
as a **vertical stripe across unrelated broadcasts**. This is the only view
that distinguishes "gawk had a bad minute" from "that broadcast had a bad
minute", and nothing else in the design can answer it (Q5).

**Trends**: bucketed aggregates over rollups — median/p95 of a chosen metric
over time, grouped by `appVersion`, delivery mode, browser, OS or resolution —
and a **cohort A/B**: two version or date ranges side by side, with a plain
statement of how thin the baseline is (`FleetSummary` already refuses to
over-claim below 5 sessions; that honesty carries over). Rollups are
permanent, so a range may far exceed the raw window — the view says when it is
answering from rollups alone (D10).

| Acceptance criterion | Verified by |
|---|---|
| A pod-wide event appears as a vertical stripe across unrelated broadcasts | test fixture |
| "Median received fps by appVersion, last 30 days" is one interaction | manual |
| A cohort comparison states its sample size and flags a thin baseline | test |
| A range beyond the raw window is labelled rollup-derived | test |
| Bucketing is server-side; a 30-day query never ships per-session rows | test |

#### TH8 — Annotations

D16's write path. Pin a note to a session, a broadcast, or a timestamp on any
timeline. Annotations render as markers on every chart covering their moment,
appear in exports, and are permanent.

| Acceptance criterion | Verified by |
|---|---|
| A note pinned at 21:04 appears on every chart whose range covers 21:04 | test |
| Annotations survive the raw-retention prune of the session they describe | test |
| A session file is never modified by an annotation | test |
| Markdown export carries annotations alongside findings | test |
| Deleting an annotation is possible and does not delete anything else | test |

#### TH9 — Dip explainer and cross-session correlation

D21. Click a dip episode and get: every metric that moved materially inside
its window, ranked by magnitude, relay and client together (largely a
projection of TM10's per-episode counter deltas) — turning *"your fps dipped"*
into *"and keyframe drops went 0→7 while ingress loss went 0→1.2%"*. Plus a
check of whether other viewers on the same broadcast dipped in the same
window, with its own confidence stated.

| Acceptance criterion | Verified by |
|---|---|
| A dip's explanation names the counters that moved, with before/after values | test |
| A broadcast-wide dip and a single-viewer dip produce visibly different explanations | test fixture |
| With one reporting viewer, the output says so instead of implying a correlation | test |
| No explanation invents a causal claim the evidence does not carry | review + test on wording |

#### TH10 — SQL console

D18, with its cost. `#/sql`: a query editor over the NDJSON partitions,
results as a table **and** feedable to the chart components so an ad-hoc query
is plottable. Gated on the flag; default on, per the owner's decision, subject
to §8 Q1's resolution.

| Acceptance criterion | Verified by |
|---|---|
| A query returns a result set and can be charted without leaving the page | manual |
| A build with no engine wired says so plainly and does not render a broken editor | test |
| `go build ./...` on a fresh clone stays cgo-free (§8 Q1's resolution) | CI |
| A malformed or long-running query fails safely with a readable message | test |

#### TH11 — Operator ergonomics

Not polish. These decide whether the page is usable at 21:00 with someone
waiting. Their criteria bind each surface as it ships.

- **Pause** (D-none, Q11). One key freezes every poll and every chart; the
  frozen instant is stated. Nothing moves while it is read.
- **Watch** (D19). Star a live broadcast; it pins and visibly changes on
  escalation. No notifications, no sound, no server state.
- **Command palette.** ⌘K to jump to a session id, a broadcast key, a code, a
  metric, or a view — the keyboard path through everything TH1 made
  addressable.
- **Absolute time everywhere historical**, with the timezone shown (D5).
- **Export**: session bundle as JSON; a chart's data as CSV; a finding as
  markdown (TH6), annotations included (TH8).
- **No layout shift while values tick**, at D14's density.
- **Bounded memory** in a tab left open for a day.
- **Background-throttle honesty.** A resumed tab marks the gap it did not
  observe; it never backfills or draws through it.
- **Mobile triage** (D17): live list, severity, verdicts and one chart work on
  a phone; dense views say they are desktop-only.
- **Budgets**: first paint < 1 s on a port-forward; 50 broadcasts × 200
  viewers stays interactive; 5 lanes × 2 000 points < 250 ms.

| Acceptance criterion | Verified by |
|---|---|
| Pause freezes every surface and names the frozen instant | test |
| A watched broadcast's escalation is visible without a notification | test |
| ⌘K reaches every view and every addressable object | test |
| Every historical timestamp is absolute, with a stated timezone | test |
| Export reopens in the UI and is readable by `jq` | test |
| A tab backgrounded 10 minutes shows the gap as a gap | manual |
| Layout does not shift as values tick, on every surface | test |
| A phone renders the triage set legibly; dense views say they are desktop-only | manual |
| Budgets above are met on the reference fixture | measured |

---

## 5. Server-side work this requires

UI requirements that ignore the API they need are wishes. All additive; every
existing default unchanged (D1).

| Need | Chunk | Note |
|---|---|---|
| `GET /v1/fields` — catalogue with kind/unit/role/description | TH5 | D8; prevents a forked field list |
| `from`/`to` window + full-resolution opt-in on `GET /v1/sessions/{id}` | TH2 | So a live detail page fetches incrementally instead of re-reading the file per tick |
| `GET /v1/broadcasts/{key}` — sessions with spans, merged events, relay series | TH4 | Only per-session detail exists today |
| `GET /v1/broadcasts/{key}/diagnose` — broadcast-scope verdicts over stored data | TH4/TH6 | `Report.Scope` already documents `"broadcast"`; only the live path computes one |
| Relay observation series for a broadcast/time range | TH4 | `ReadRelay(date)` is whole-date only |
| Cursor pagination + server-side sort on the list endpoints | TH3 | Today: `limit` and a clamp |
| `groupBy=appVersion` + a bucketed trend endpoint | TH7 | `groupKey` has browser/os/resolution/deliveryMode |
| Fleet timeline projection (severity bands per broadcast over a range) | TH7 | New |
| `GET /v1/rules` — catalogue; per-session trace on diagnose | TH6 | D20, read-only |
| Annotations store + CRUD (permanent, beside rollups) | TH8 | D16, the only write path |
| DuckDB engine wired to `mcp.Options.SQL` + an HTTP query endpoint | TH10 | D18 — see §8 Q1 before building |
| SSE `/live/stream` with the existing poll as fallback | all live views | D22 |
| `-retention-days` chart default 14 → 30 | — | D15; chart + README only |

Costs worth stating rather than discovering: `GetSession` parses every line of
a session file per call (a 4-hour session at 2 s ≈ 7 200 lines) — fine for a
human click, unacceptable on a 2 s poll, which is what the `from`/`to` window
is for. `FindSession` scans date partitions newest-first, so an old id walks
the tree; acceptable for a click, and the reason a session id should carry its
date from the listing that already knows it.

---

## 6. Non-goals

- **No new measurement, no new client-side collection** (D3, R28 §7).
- **Not a Prometheus replacement.** Fleet health and alert-shaped questions
  stay Prometheus's; this is per-session forensics beside it.
- **No alerting, paging or notification infrastructure.** D19's in-tab watch
  is the whole of it: no rules, no server state, no browser notifications.
- **No rule tuning from the UI** (D20) — transparency without mutation.
- **No control actions.** Nothing here kicks a viewer, restarts a pod or
  changes a stream. Annotations are the only write, and they describe rather
  than act.
- **No public exposure of the read surface**, and no external asset fetch
  (D7).
- **No PII, no new identity.** The obfuscated key stays the only broadcast
  identity the UI handles; `/v1/resolve` stays POST-only, off by default,
  never logged.
- **No auth/RBAC model.** The read listener's basic auth and non-public
  posture remain the boundary.
- **Grafana.** Deferred again by owner decision (2026-07-29); R9 M8 stays
  open, and TH7 is now the home for trends.

---

## 7. Risks

- **Scope.** Eleven chunks is a lot. Mitigation: the wave structure, and the
  fact that TH1+TH2 alone close Q2 and the dead-link defect and are worth
  shipping on their own.
- **D18's cgo dependency** is the one decision that changes the module's build
  properties — zero third-party deps and `CGO_ENABLED=0` into
  `distroless/static` today. §8 Q1 must resolve before TH10 starts.
- **ECharts' imperative lifecycle inside React** is the classic source of
  leaked instances and stale closures on hot reload. Mitigated by owning the
  wrapper hook (D11) rather than inheriting one, and by a test that mounts and
  unmounts every chart surface.
- **Density versus honesty.** D14's console packs more onto a screen; the
  no-layout-shift and absence-is-never-blank rules get harder to hold, not
  easier, and they are exactly what a dense grid tempts you to break.
- **A second surface for the same rules.** R28 §8 already names the cost of
  the dashboard sharing `diagnose()`'s engine: a bad rule is wrong in two
  places. TH6 and TH9 make rule output *more* visible, raising the cost — and
  are also the mitigation, because evidence, provenance and a trace on screen
  are what let a human catch a rule that is confidently wrong.
- **TH9's cross-session correlation can manufacture a causal story.** Two
  viewers dipping together is evidence about a shared leg, not proof of one.
  Mitigated by stating confidence and by wording that reports co-occurrence
  rather than cause — a reviewable acceptance criterion, not an intention.
- **Reading session files on a human-triggered path** is the one place this
  spends I/O the live page deliberately does not. Bounded by being per-click
  and windowed; the criterion asserting `/live` still opens no session file is
  what keeps the boundary from eroding.
- **Trend queries over permanent rollups** grow with fleet age, not with the
  raw window. Server-side bucketing (D4) is the bound.

---

## 8. Open questions

1. **D18's build cost.** Does the deployed image take a cgo DuckDB dependency
   and leave `distroless/static`, with `go build` on a fresh clone staying
   cgo-free and reporting the tool unavailable (this document's
   recommendation) — or does the console stay behind a default-**off** flag as
   R28 D11 had it? The owner has chosen default-on; this asks only how to pay
   for it.
2. **D22 (SSE) is my call, not the owner's.** Flagged for objection: it adds
   an endpoint and reconnect logic in exchange for not re-sending the fleet
   projection every 2 s at D12's scale.
3. **How much of TH4's relay lane earns its keep at replicas 1?** Re-home
   markers matter at replicas ≥ 2; at replicas 1 the lane is ingress loss and
   drop counters, which is still useful but much smaller.
4. **Does TH7's fleet timeline want a Prometheus overlay later** — relay
   CPU/memory/pod restarts on the same axis? Out of scope now, but the axis is
   the natural place for it, and knowing that shapes the component's seam.
