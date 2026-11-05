# R36 — Telemetry UI usability pass and facelift (docs/86)

**Status**: implemented 2026-11-05, chunks **TU1–TU12** (§4). The relay
half of TU5 shipped first, in #714. Built against the Claude Design canvas
[gawk telemetry facelift (R36)](https://claude.ai/artifact/UuTW23kL5wtEJZWYCD6e8x)
(eight boards, private to the owner), and checked against the production
read API on 2026-11-05 (§5).

**Relationship to earlier work**: R31 (docs/36) built the diagnosis SPA and
settled its information architecture: sections, routes, URL state, Live as
the landing view (UD13), and a dense dark console with hue reserved for
severity (UD14). All of that stands. ROADMAP.md §R36 lists the frictions with
file-and-line evidence; this doc records what was decided and built.

---

## 1. What "done" means

The dashboard answers the same questions as before. It now reads without
fighting the operator:

- Every table fits its content and its container, and the operator can
  resize any column.
- A lane stack reads through one time axis and one fixed readout column.
- No text is drawn inside a lane.
- A range older than the service is drawn as the service's age, not raised
  as a warning.
- A striped viewer is one participant with its legs beneath it, and a viewer
  served by a PoP says so.
- It has a visual layer: IBM Plex, a four-step surface scale, severity
  pills, and a light theme from the same tokens.

R31's no-layout-shift rule (TH11) and its absence rules (UD9, UD10) bind every
change below.

## 2. Decisions

**D1 — Dark by default, light and "system" on request, resolved in JS.**
UD14 said dark by default; R31 actually followed the OS. The theme is now a
per-browser preference (`localStorage`), cycled from the header. It is
resolved in `state/theme.ts` and written to `<html data-theme>` before the
first render. It is resolved in JS rather than in a media query because the
charts paint on a canvas. A canvas cannot read CSS, so `charts/palette.ts`
reads the computed tokens for the resolved theme and the chart options use
those strings. No `.tsx` file may carry a colour literal: R31's lane veils
were `rgba(11,12,14,0.55)` and drew black on a white chart.

**D2 — IBM Plex, bundled, from IBM's own unmodified subsets.** UD7 forbids
fetching a font. Five latin-subset woff2 files (Sans 400/500/600, Mono
400/500, about 101 kB) are vendored into `ui/src/assets/fonts/`.
- **Why vendored:** they are copied from `@ibm/plex-sans` 1.1.0 and
  `@ibm/plex-mono` 2.5.0 rather than installed, because those packages run
  an install-time telemetry hook.
- **Licence:** OFL-1.1. It allows bundling with software and asks that the
  licence travel with the fonts: `OFL.txt` sits beside the files and is
  reproduced in `ui/THIRD-PARTY-NOTICES.md`.
- **Reserved name:** "Plex" is reserved only for modified versions. That is
  why these are IBM's subsets, not ours.
- **Licence check:** OFL is not on the npm allowlist, because the fonts are
  not a lockfile dependency. `tools/licenses/gen-notices.py` lists them by
  hand, as it does RNNoise.
- **Fallback:** glyphs outside Latin-1 (○ △ ● ★ ⌘) fall back per glyph to
  the system font.
- **Build:** Vite emits the fonts under their own names, and the CSP's
  `font-src 'self'` already admits them.

**D3 — Column widths: measured once per load, frozen, operator-overridable.**
This answers ROADMAP §R36 item 1 and its first two design questions.
- **Measuring.** `lib/columns.ts` measures each column's text with a canvas
  against the bundled fonts. This happens when the *data load* changes: a
  new range or filter, or a joiner or leaver on a live table. A tick never
  triggers it.
- **Ticking cells.** A ticking cell is sized by a `samples` string (its
  widest form, e.g. `'144 → 144 / 144'`), so a value growing from `9 ms`
  to `142 ms` never re-flows the row.
- **Why a canvas.** A canvas, not the DOM, is what lets the virtualized
  history grid stay a grid. It is measured over every loaded row, not just
  the rendered window, so scrolling never re-measures.
- **The free-text column.** One free-text column per table (verdict) takes
  the remainder and is the only one that ellipsises; its title carries the
  whole value.
- **Fitting the container.** When the measured sum would overflow the
  container, columns marked `squeeze` (client, app version) give width down
  to a floor, in proportion to what each can give. R31's percentage
  colgroups existed to guarantee the fit; this keeps the guarantee without
  guessing.
- **Operator overrides.** Every boundary has a grip: drag to resize,
  double-click (or Enter) to fit to the uncapped content, arrow keys to
  nudge. Overrides persist per table in `localStorage`, not the URL, which
  eight widths would make unpasteable. An override is never squeezed. If
  the table still does not fit, it scrolls sideways rather than cutting a
  column off.
- **Reset.** *reset widths* drops the overrides, and the measured layout
  returns.
- **Static tables.** Findings evidence, dip movers and the dips table are
  static per load, so they use the browser's auto layout. Content-fit costs
  nothing there and nothing ticks.

**D4 — Chart stacks: one axis, one readout rail, no floating tooltip.**
This answers ROADMAP §R36 item 2. The broadcast lanes and the session
timeline are *stacks*: ECharts instances joined by `echarts.connect()`,
read through `charts/stack.ts`.
- **The stack's state.** The stack owns the crosshair instant and the
  visible window.
- **The axis.** It is drawn once, as HTML: sticky above the broadcast
  lanes, below the session charts.
- **The readout rail.** Each lane's value at the crosshair sits in a fixed
  column (`valueAt`). It is absent, "—", when the nearest earlier point is
  older than two cadences, or one and a half point spacings on a
  downsampled lane (UD9).
- **Snapping off.** The crosshair must follow the mouse:
  `xAxis.axisPointer.snap: false`. ECharts forces snapping on from the
  tooltip, and over a viewer who had left it dragged the whole stack's
  instant back to that viewer's last sample.
- **Zoom.** Zoom is relayed explicitly to every member. `connect` relays an
  inside-zoom but not the slider's, and the relay stops on its own echo.
  Every member also reads the stack's zoom whenever its option is rebuilt
  (new data, a theme switch, a re-render). `setOption` replaces the option
  outright, and a rebuild that left the zoom out reset that one member while
  its neighbours stayed zoomed (round-1 review).
- **Single charts.** Explore, the fleet trend and SQL keep a tooltip,
  pinned to the plot's top-right corner and confined.

**D5 — No text inside a lane.** On production, overlapping `markArea` and
`markLine` labels merged into runs like *"taltatdipbhidipdip…"*.
- **What is drawn.** A band is a fill: hatched for "not here / could not
  see", the bad tint for a dip. A re-home or a note is a bare dashed line.
- **Events.** A stored event is a short tick on the lane's floor.
- **Where the names go.** They live in the readout rail, which names the
  event or band under the crosshair, and in the legend.
- **The one exception.** An empty plot may say why it is empty (UD10).

**D6 — Coverage says why; the service's age is an axis extent.** This
answers ROADMAP §R36 item 3 and its design question. `store.Prune` deletes
raw partitions only, so the first rollup is the service's first day.
- **The field.** `Coverage.reason` is `before-data`, `raw-pruned` or
  `none`. The `note` text is unchanged for the MCP surface.
- **`before-data`.** The UI says nothing as a warning. It shows *"data since
  31 Oct"* beside the range control, and a hatched *"not installed yet"*
  span on the fleet stripes and trend.
- **`raw-pruned`.** One quiet line: *"raw samples older than N days are
  pruned; rollups and verdicts remain."*
- **Both at once.** A range past both edges reports `before-data`. The UI
  still adds the retention line when `rawFromMs > rollupsFromMs`.
- **Never dismissible.** The note still cannot be dismissed. What changed
  is when it fires.

**D7 — A striped viewer is one participant; its legs are joined by a keyed
owner, not stored twice.** This is ROADMAP §R36 item 4, with its corrected
premise.
- **The relay key (#714).** The relay publishes
  `subscriberDetails[].stripeOwner`:
  `hex(HMAC-SHA256(statsKey, "stripe-owner\0"+token)[:6])`.
  - Because the stats key is fleet-shared, a viewer's primary and legs carry
    the same key on every pod.
  - It is minted per transport attempt, so a reconnect gets a new one.
  - It is never present on PoP observations, and never when striping is off.
- **Storage.** No new record kind. The broadcast record already stores
  every subscriber entry, legs included, so the scraper decodes the stripe
  fields onto it. Separate leg records would have stored every leg twice.
- **Live view.** `SessionView.legs[]` (member, n, pod, role, drops over the
  last scrape window, queue depth, age) and `BroadcastView.stripeLegs`, the
  count of legs held, attributed or not.
- **Timeline.** `Lane.legs[]`: dropped datagrams per second per scrape
  interval, at most 16 leg sessions per lane.
- **Counts.** Legs never consume a lane (`MaxLanes`), and stay out of every
  count and baseline (#657's `IsAudience`).
- **On screen.** The UI renders a `striped ×N · K pods` tag and the legs as
  sub-rows or sub-lanes.
- **No sent counter.** The relay keeps no per-subscriber sent counter, so
  there is none.

**D8 — A PoP-served viewer is labelled, not re-read.** The live
`SessionView` and a timeline `Lane` carry `pop` (the PoP's display name)
beside `issuer`. The broadcast view still reads home relay lines only. A
PoP viewer's lane says so with a tag, and an empty relay reading says
*"its relay figures are on the PoP"*.

**D9 — Sparklines from their own endpoint, never the SSE snapshot.**
`GET /live/sparks?broadcast=<key>` returns a 2-minute tail per session.
- **Polling.** It is polled every 2 s only while that broadcast's card is
  open and the page is not paused.
- **What a value is.** Each value is the lowest sample in its bucket, and a
  `null` is a gap (UD9).
- **Step.** It adapts: the narrowest width dividing 120 s that is at least
  1.6× the broadcast's median sample gap, which is 4 s at default rates.
- **Grid.** The window ends at the step boundary at or after the poll, so
  bucket edges hold still between polls.
- **`throughMs`.** It marks buckets not yet uploaded, as distinct from a
  real gap.
- **Source.** A bounded per-session ring in the live projection, so no
  files are read per poll.
- **Why not the snapshot.** A 60-float tail per session per tick would have
  broken UD1's bounds on a hot broadcast.

**D10 — Thin trend buckets are drawn hollow; confident ones as points.**
This answers the design question.
- **Thin buckets.** A bucket under five sessions still breaks the line. It
  is drawn as a hollow, unconnected circle: present, claiming nothing.
- **Confident buckets.** They are marked with solid points. On a sparse
  fleet a confident bucket often has no neighbour, and a bare line draws
  nothing for it. The production trend on 2026-11-05 had exactly one, and
  R31 showed an empty plot.
- **All thin.** When every bucket is thin, the plot explains itself in
  place.

**D11 — A quiet stream is not a stale one, and a pause loses nothing.** This is a bug fix, test-first.
- **The cause.** The SSE stream sends only changes, plus a keepalive every
  20 s. R31 judged it by the poll's 6 s threshold, so every idle fleet read
  "feed stale" six seconds after going quiet. The new header shows feed
  health on every page, which made it visible everywhere.
- **The fix.** Any received bytes (a keepalive included) refresh the feed,
  and stream mode uses a 25 s threshold.
- **Pause.** For the same reason a change that arrives while the page is
  frozen is held, not dropped, and shown on resume: a quiet stream would
  never send it again. The indicator re-checks every 2 s,
  because staleness is time passing with nothing arriving, which changes no
  store.

**D12 — One clock.** Every time of day is on a 24-hour clock. An axis
saying 21:00 beside a table saying 09:00 PM made the reader do arithmetic.

**D13 — The session page is two columns, and dips are one row each.**
- **Layout.** The verdict, timeline and dips sit on the left; identity, the
  fleet comparison (bars with the median marked) and notes on the right.
- **Charts.** Each is titled by the question its unit answers (*"Did frames
  arrive, decode and render?"*). The four most useful are open, and the
  rest are one click away.
- **Dips.** Each episode is a row: when, how long, how deep, its three
  largest movers, and who else dipped. A row opens to every mover and the
  co-occurrence sentence.
- **Result.** The production session that measured about 5,900 px is now
  about 1,750 px.

**D14 — The phone gets triage rows.** Below 720 px (UD17):
- **Sessions.** A live card's sessions become tappable rows, worst first,
  each with its sparkline. The healthy rest fold into one line.
- **Header.** The tabs become pills.
- **Removed.** Desktop detail (the pod, rule ids) goes.
- **Desktop-only views.** They still say so.

## 3. Bundle cost

Measured against `main` at 7fc677aa (`npm run build`):

| | main | R36 | Δ |
|---|---|---|---|
| `app.js` | 933.5 kB (303.6 kB gz) | 969.3 kB (315.3 kB gz) | +35.8 kB |
| `app.css` | 30.9 kB | 47.2 kB | +16.3 kB |
| fonts | — | 100.6 kB (5 × woff2) | +100.6 kB |

That is about 153 kB in all: the fonts land inside the 100–150 kB the roadmap estimated for them,
and the code adds about 52 kB. The legend component left the ECharts
registration, since legends are HTML now.

## 4. Chunks and acceptance criteria

| Chunk | What | Accepted when | Verified by |
|---|---|---|---|
| **TU1** Foundations | Tokens (dark + light), Plex bundled, theme preference, header (brand, tabs, search, feed, clock, pause, theme), severity pills, panels, controls | No colour literal in `src/**/*.tsx` outside the fallback table in `palette.ts`; both themes render every view; the no-external-fetch and CSP tests pass on the built bundle | `go test ./internal/dashboard/` after `npm run build`; screenshots of every view in both themes (§5) |
| **TU2** Column widths | `lib/columns.ts`, grips, drag line, reset, squeeze; history and room grids, live table, SQL results | Headers align with their columns; no cell but the free-text one truncates at measured width; a drag persists across reload; reset drops it; a tick never changes a width; the live table fits its card | `lib/columns.test.ts`; production History screenshot (header drift gone, "broadcaster" and pills whole); headless drag → `localStorage` holds the width |
| **TU3** Chart stacks | `charts/stack.ts`, `StackChart`, `TimeAxis`, `ZoomBar`; broadcast lanes, session timeline; pinned tooltip on single charts | One axis per stack; readouts follow the crosshair and show "—" where a lane had left; the slider zooms every lane; no label text inside a lane | `charts/stack.test.ts`; headless hover and zoom on a production broadcast (`30.0 / — / — / 30.2 / 30.0` at 21:08:42) |
| **TU4** Coverage reason | `Coverage.reason` server-side; `CoverageNote`, `DataSince`, not-installed hatch | `before-data` is silent; retention is one quiet line; every `coverage()` caller sets the reason | `internal/readapi/coverage_test.go`; `facelift.test.tsx` (CoverageNote cases) |
| **TU5** Stripe legs | Relay `stripeOwner` (#714); scrape decode; live `legs`/`stripeLegs`; lane `legs`; UI sub-rows and sub-lanes | A striped viewer is one row with N legs; legs count nowhere (`subscribers`, viewer counts, `peerMedianDropped`, `viewerOf`, #669's repair); legs never take a lane | gawk-server hub/transport tests (#714); `internal/live/legs_test.go`, `internal/readapi/legs_test.go`, `relayscrape/stripe_test.go`; `facelift.test.tsx`; fixture screenshot |
| **TU6** PoP label | `pop`/`issuer` on live sessions and timeline lanes; UI tags | A PoP-served viewer carries a PoP tag; its empty relay reading says why | `internal/readapi/pop_lane_test.go`; `facelift.test.tsx` |
| **TU7** Sparklines | `/live/sparks`, ring in the projection; `Sparkline`, `useSparks` | A 2-minute tail per live row; gaps break the line; no tail reads "—"; polled only while the card is open | `internal/live/sparks_test.go`, `internal/readapi/sparks_test.go`; `facelift.test.tsx`; fixture screenshot |
| **TU8** Session page | Two columns, question titles, legend values at the crosshair, `DipTable` | Dips are one row per episode; the production session is under 2,000 px tall | production Session screenshot (§5) |
| **TU9** Fleet | Stripes with gridlines and labels, not-installed hatch, hollow thin and solid confident buckets, in-plot explanation, cohort arms | The production trend draws every bucket; slider below the axis labels | production Fleet screenshot |
| **TU10** SQL | Ask panel, highlighted editor with line numbers, resizable results, cell popover with copy | A truncated cell opens in full on click or Enter; existing Ask tests pass | `SqlView*.test.tsx`; `facelift.test.tsx` (tokenizer) |
| **TU11** Phone | Pill tabs, compact header, `PhoneSessions` | A live card on 390 px shows worst sessions as rows with sparklines | fixture screenshot at 390 px |
| **TU12** The rest of item 6 | Fluid id inputs (Explore, Rules), header chips instead of truncated slots, content-sized field picker, 24-hour clock, the unused `.table` deleted, docs/36:298 pointer, D11 | Each listed friction is gone | `liveStore.test.ts` (D11); screenshots |

## 5. Verification log (2026-11-05)

- **Production data.** The dev server ran with
  `GAWK_TM_TARGET=https://gawk-telemetry.kube.ioio.fi` and the read
  credential, and headless Chromium captured every view in both themes and
  at 390 px.
- **Before the new endpoints were deployed.** Production served neither
  `/live/sparks` nor `reason`, so the sparkline column read "—" and the
  coverage note fell back to its R31 text. Both are covered by tests
  instead.
- **Interaction.** Hover and zoom were driven on broadcast `d0c507951d3d`;
  a column drag was driven on History.
- **Live page.** Production had no live broadcast, so it was captured
  against a fixture snapshot: a bad card with a striped viewer (3 legs on 2
  pods), a PoP viewer and an unreported viewer.

## 6. Open, deliberately

- **Explore, Rules and Room** got the shared furniture but no board of
  their own; they reuse the history row and panel patterns.
- **A PoP's own stripe legs** stay unattributable. A PoP keys its stats
  with its own key, and re-keying at home would need the raw token on the
  link (#714).
- **The broadcast verdict** is absent whenever `/v1/broadcasts/{key}/diagnose`
  answers 404 (seen on an ended production broadcast). That is the R31
  behaviour and is left as it was.
