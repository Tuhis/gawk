# Roadmap

High-level roadmap for the work that follows v0.5. The v0.x milestones
(loopback → hello-world → single-client → fan-out → resilience + deploy,
chunks A1–D4) are complete — see [`docs/implementation-tasks.md`](docs/implementation-tasks.md)
and the [Status section of the README](README.md#status). This document is
the equivalent forward-looking view: what we build next, in what order, and
why.

**How this document is used**: each item below is deliberately high-level.
When an item is picked up, it gets its own numbered design doc continuing the
existing `docs/NN-*.md` series (milestones, chunked tasks, acceptance
criteria — same shape as `docs/implementation-tasks.md` used for the server).
This file then just tracks per-item status and links to that doc. Ordering is
a proposal based on dependencies and risk, not a contract — but reordering
should be a conscious decision, since later items assume earlier ones (R2's
limits are per-broadcast, R4 builds on R3's ladder, R6 presents whatever
feature set exists).

| # | Item | Status |
|---|------|--------|
| R1 | [Multi-broadcaster support](#r1--multi-broadcaster-support) | ✅ done ([docs/06](docs/06-multi-broadcaster.md)) |
| R2 | [Hardening](#r2--hardening) | ✅ done ([docs/07](docs/07-hardening.md)) |
| R3 | [Broadcaster resolution & framerate picker](#r3--broadcaster-resolution--framerate-picker) | ✅ done ([docs/08](docs/08-resolution-framerate-picker.md)) |
| R4 | [Automatic resolution fallback](#r4--automatic-resolution-fallback) | ✅ done — manually verified 2026-07-14 ([docs/09](docs/09-automatic-fallback.md)) |
| R5 | [Viewer live-edge enhancements](#r5--viewer-live-edge-enhancements) | 🚧 Q1–Q3 implemented 2026-07-14 (re-scoped); Q4 measurement pass + manual verify pending ([docs/15](docs/15-viewer-live-edge.md)) |
| R6 | [Production UI](#r6--production-ui) | ✅ done (J1–J6); manual browser verify passed 2026-07-14 ([docs/10](docs/10-production-ui.md)) |
| R7 | [Hardware-supported controls & capture constraints](#r7--hardware-supported-controls--capture-constraints) | not started |
| R8 | [Worker Offloading & Reliable Keyframes](#r8--worker-offloading--reliable-keyframes) | ✅ done (S1–S7: reliable keyframes + worker offload); browser-verified 2026-07-14 ([docs/12](docs/12-worker-and-reliable-keyframes.md)) |
| R9 | [Observability & metrics](#r9--observability--metrics) | ✅ done (M1–M7); manually verified 2026-07-14; M8 (Grafana) still deferred ([docs/13](docs/13-observability.md)) |
| R10 | [Viewer render performance](#r10--viewer-render-performance) | ✅ done — P1–P3 + decoder-queue bump + field-finding fixes (keyframe wait 1 s, relay zombie eviction) implemented and re-verified on Chrome + Firefox 2026-07-14 (P4 remainder deferred) ([docs/14](docs/14-viewer-render-performance.md)) |
| R11 | [Broadcaster worker offload](#r11--broadcaster-worker-offload) | 🚧 implemented 2026-07-14 (K1–K4); automated gates green, manual browser verify pending ([docs/16](docs/16-broadcaster-worker-offload.md)) |

---

## R1 — Multi-broadcaster support

**Goal**: multiple simultaneous, independent 1-to-many sessions. Five friends
can each stream to their own audience at the same time. Starting a broadcast
mints a short, shareable **broadcast ID**; the broadcaster's UI shows the ID
and a direct join link (`#/view/<id>`), and viewers join by pasting the code
or clicking the link.

**Why first**: this reshapes the hub, the routes, and the URL scheme that
everything else builds on. Hardening limits (R2) are per-broadcast,
`/statusz` becomes per-broadcast, and the production UI (R6) is designed
around the join-by-code flow. Doing R1 later means reworking all of them.

**Scope sketch**:

- The hub (`gawk-server/internal/hub`) becomes a registry of independent
  broadcast sessions keyed by broadcast ID. Everything that is currently
  singleton state — publisher slot, subscriber set, cached decoder config,
  cached last keyframe — becomes per-broadcast. The existing semantics carry
  over unchanged *within* a broadcast: new-publisher-session invalidates that
  broadcast's caches, caches persist while its broadcaster is merely away,
  slow subscribers get drops not blocking.
- **Broadcast IDs are ephemeral and per-session**: a new code is minted each
  time a broadcast starts and dies when the broadcast ends. Short enough to
  read out loud (~6 characters), from an unambiguous alphabet (no `0/O`,
  `1/l/I`). With ~5 concurrent broadcasts the space is nowhere near
  exhaustion; unguessability is what matters (see R2 access-control note).
- Routes grow an ID dimension: publish targets a specific broadcast,
  subscribe requires one. `409 publisher exists` becomes per-broadcast.
- Frontend: broadcast page surfaces the ID + copyable join link; view page
  accepts an ID from the URL hash or a text field. `ViewerSession`
  reconnect logic re-joins the same broadcast ID.

**Key design questions** (for the design doc):

- **How the ID reaches the broadcaster at session setup.** The WebTransport
  JS API exposes no response headers or body on the CONNECT, so the server
  can't mint the ID in the CONNECT response. Two candidate shapes:
  client-mints the ID and publishes to `/publish/<id>` (server rejects
  collisions — fine, they're ~impossible), or a tiny HTTPS
  `POST /broadcasts` mint endpoint called before CONNECT. Client-minted is
  simpler; the mint endpoint gives the server control (and a natural place
  for the R2 broadcast secret). Decide in the design doc.
- **Broadcast lifecycle when the publisher drops.** Today "broadcaster away,
  caches persist" is the desired behavior for a relay restartable
  broadcaster. Per-broadcast, an abandoned session must eventually be
  garbage-collected (timeout after publisher disconnect with no return) or
  the registry leaks. Viewers of a GC'd broadcast need a terminal state, not
  infinite reconnect.
- What `/statusz` looks like with N broadcasts (see R2).

**Non-goals**: persistent per-broadcaster channels ("juho's channel" with a
stable bookmarkable ID surviving restarts) — deliberately deferred; it needs
an identity/persistence story the ephemeral design avoids. Noted as a
possible follow-up, not part of R1.

**Status**: Implemented and fully verified (2026-07-12) — see [`docs/06-multi-broadcaster.md`](docs/06-multi-broadcaster.md) progress table.

## R2 — Hardening

**Goal**: the relay enforces explicit limits and a minimal trust model
instead of relying on obscurity and good behavior, so a bug, a rogue client,
or an accidentally-shared URL can't take the homelab down.

**Why here**: limits are naturally per-broadcast and per-server, so this
lands right after R1 defines what a broadcast is. It's also a prerequisite
for being comfortable leaving the service running unattended.

**Scope sketch**:

- **Configurable limits**: max concurrent broadcasts (default ~5), max
  viewers per broadcast (default 15, extending today's global 429 semantics),
  connection-attempt rate limiting. All via flags + Helm values, matching the
  existing pattern in `gawk-server/internal/config`.
- **Access control — shared secret to broadcast**: publishing requires a
  pre-shared key configured server-side (Helm value / secret) and entered
  once in the broadcaster UI. Viewing stays gated only by knowing the
  unguessable broadcast ID — acceptable for a ~15-friend audience, and
  losing a viewer secret ceremony keeps join friction near zero.
- **Resource bounds**: cap memory held per broadcast for the cached keyframe
  + decoder config (a hostile or buggy publisher shouldn't be able to grow
  them unboundedly), and bound total registry size via the broadcast limit.
- **Defensive parsing**: strict validation limits on datagram headers
  (chunk counts, lengths, config sizes) — malformed input drops the datagram
  or the session, never panics or allocates unboundedly.
- **Observability**: `/statusz` reports per-broadcast stats (viewer counts,
  drop rates, cache state) plus server-wide totals, so limit tuning is
  data-driven.
- **Bandwidth limits**: configurable cap on bandwidth consumed by the relay
  server instance, ensuring server admin can stay within agreed bandwidth
  consumption. What to do when limit is reached is left to the design
  document.

**Key design questions**: where the broadcast secret is presented (CONNECT
header vs. the R1 mint endpoint — these two designs should be decided
together); whether rate limiting lives in the relay or is better done at the
homelab edge; what, if anything, needs to change in the Helm charts' defaults
for a "safe by default" install.

**Non-goals**: real user accounts, tokens, or a login flow — overkill for
the audience. DDoS-grade protection — this is a private homelab service, not
a public platform.

**Status**: done.

## R3 — Broadcaster resolution & framerate picker

**Goal**: the broadcaster chooses what they send. Resolution: **native /
1080p / 720p / 480p**. Framerate: **native / 60 / 30 / 5 fps**. Native
remains the default; the lower rungs trade fidelity for encode headroom and
bandwidth (5 fps is the "screen-sharing a menu, saving my uplink" mode).

**Why here**: independent of R1/R2 (it's broadcaster-side media pipeline
work) but a hard prerequisite for R4, which automates stepping down the same
ladder. Building the picker first means R4 only adds the *decision*, not the
machinery.

**Scope sketch**:

- **Scaling happens broadcaster-side, before encode.** WebCodecs
  `VideoEncoder` does not scale — a scaling stage goes into
  `gawk-app/src/media/` between capture and encoder, likely
  `VideoFrame → OffscreenCanvas (or createImageBitmap resize) → new
  VideoFrame`. Requesting a lower capture resolution from `getDisplayMedia`
  instead is *not* the plan: we keep capture at native and scale ourselves,
  so the ladder can change mid-stream (R4) without renegotiating capture.
- **Framerate limiting = timestamp-based frame dropping before encode** —
  skip frames until the inter-frame interval matches the target. No capture
  renegotiation here either.
- Selected resolution means "cap the longer dimension at the rung's 16:9
  width equivalent (1080p → 1920), preserve aspect ratio" — ultrawide
  3440×1440 at 1080p becomes 1920×804, not a 16:9 squeeze. Capping the
  longer dimension (not the shorter) keeps total pixel count bounded even
  for pathological aspect ratios like 25000×1080.
- Encoder reconfiguration on ladder change (new config → new keyframe →
  viewers' decoders reconfigure via the existing DecoderConfig re-emit path —
  this flow already exists for late joiners and should be verified for
  mid-stream config changes).
- UI: picker on the broadcast page, disabled/locked once R6 decides where
  settings live.

**Key design questions**: cheapest scaling path that stays off the main
thread (OffscreenCanvas in the existing worker vs. main-thread canvas);
whether 480p at 60 fps is worth allowing or the matrix should be constrained;
how a mid-stream resolution change interacts with the relay's cached
keyframe (stale-resolution keyframe priming a new viewer for one GOP).

**Constraints to respect**: the project rule from `docs/01` is doubly
important here — derive all dimensions from the actual `VideoFrame` in hand,
never `getSettings()`; and keep H.264's even-dimension requirement when
computing scaled sizes.

**Status**: done — implemented and manually verified 2026-07-13 (chunks
H1–H4) — see
[`docs/08-resolution-framerate-picker.md`](docs/08-resolution-framerate-picker.md).

## R4 — Automatic resolution fallback

**Goal**: if the broadcaster's machine can't sustain encoding at the current
resolution, step down the R3 ladder automatically instead of stuttering or
dying — and step back up when the machine recovers. The broadcaster left the
default ("auto") on a laptop that can't do native — the stream should
degrade to 720p on its own, with a visible indication, rather than requiring
the broadcaster to diagnose encoder backpressure themselves.

**Why here**: pure delta on top of R3 — the ladder, the scaling stage, and
mid-stream reconfiguration all exist; R4 adds detection and the decision
loop.

**Scope sketch**:

- **Detection**: sustained `VideoEncoder.encodeQueueSize` growth, encode
  errors, or output falling persistently behind the target framerate. Needs
  hysteresis — a one-off spike (scene change, background task) must not
  trigger a downgrade.
- **Action**: step down one rung (resolution first; possibly framerate as a
  later rung), reconfigure via the R3 machinery, notify the UI ("streaming
  at 720p — encoder can't keep up at 1080p").
- Auto-stepping exists only in a new **"auto" resolution selection** (the
  default). An *explicit* rung is honored unconditionally — no stepping in
  either direction; frame drops are preferred over going against the
  broadcaster's stated choice (a passive "encoder can't keep up" warning is
  the only feedback there).

**Key design questions**: whether to ever step back **up** automatically
(risk: oscillation; a conservative option is "step down automatically,
step up manually" for v1 — decide in the design doc); exact
detection thresholds and window sizes (needs empirical tuning on the real
gaming PC); whether bitrate reduction should be a rung on the ladder before
resolution drops.

**Non-goals**: per-viewer adaptation (simulcast/SVC-style "each viewer gets
what their downlink supports") — that's a different architecture (the relay
is a byte forwarder by design) and explicitly out of scope.

**Status**: done — implemented + released 2026-07-13, manually verified
2026-07-14 (chunks I1–I3 + automated
gates) — see [`docs/09-automatic-fallback.md`](docs/09-automatic-fallback.md).
The design resolved the open questions above: detection is the encode-queue
rejection ratio with a sliding window + cooldown; stepping is
resolution-only, lives behind a new "auto" selection (the default), and goes
**both down and up** there — step-up probes carry exponential backoff so
steady overload can't pump quality; explicit rungs are never stepped;
bitrate is not a fallback rung. Zero relay-server, wire-format, and viewer
changes. **Real-hardware caveat**: the auto step-down could not be induced on
the gaming PC's hardware encode path — hardware encoders don't surface
backpressure via `encodeQueueSize`, so the rejection signal under-fires
there; observed perf limits were source-side (4K capture), a correct no-op.
Software-path verification + threshold tuning completed 2026-07-14 (see the
doc's "Manual verification findings"); a hardware-strain signal remains a
possible deferred follow-up.

## R5 — Viewer live-edge enhancements

**Goal**: viewers never fall behind live. **Playing the latest frames is
strictly more important than playing every frame** — this has been the
project's transport philosophy from day one (datagrams, drops over stalls);
R5 extends it through the viewer's decode/render pipeline, which today can
still accumulate latency under load.

**Why here**: benefits from R3/R4 first reducing pathological input (a
struggling encoder produces exactly the bursty, gappy stream that stresses
the viewer), and the design doc should measure before building — some of
this may already be adequate.

**Scope sketch**:

- **Latest-frame-first rendering**: if multiple decoded frames are pending,
  render the newest and drop the rest — never queue decoded frames for
  smooth-but-late playback.
- **Backlog discard + skip-ahead**: if reassembled-but-undecoded frames pile
  up (slow decode, tab throttling, network burst after a stall), discard the
  backlog. Because inter-frames can't decode without their predecessors,
  skipping means jumping to the **next keyframe** — drop everything until
  one arrives, then resume. (The viewer already does this for the *lost-frame*
  case — freeze-on-gap, CLAUDE.md build-order item 11: a non-contiguous delta
  waits for the next keyframe rather than decoding corruption. R5 generalizes
  the same drop-to-keyframe move to the *backlog/slow-decode* case.)
- **Live-edge measurement**: expose glass-to-glass / capture-to-render delta
  in stats (the wire format already carries capture timestamps) so "are we
  at live?" is observable, not vibes. Feeds the R6 debug overlay.
- Recovery-time bound: after any stall (reconnect, tab unfocus), time back
  to live is bounded by the keyframe interval (currently a 500 ms time-based
  GOP, `keyframeIntervalMs`) — evaluate whether that interval is right once
  skip-ahead exists.

**Key design questions**: whether a **viewer→server keyframe-request signal**
is worth adding so a behind viewer doesn't wait up to a full GOP for the next
keyframe. Today the data flow is strictly one-way (broadcaster → relay →
viewers) and the relay deliberately doesn't talk back to the broadcaster;
a request channel is a real protocol addition (new wire message, relay
aggregation/debouncing so 15 viewers can't spam the encoder). May be
overkill given the existing cached-keyframe priming — the design doc should
start with measurements.

**Status**: re-scoped + designed 2026-07-14, **Q1–Q3 implemented the same
day** — see [`docs/15-viewer-live-edge.md`](docs/15-viewer-live-edge.md);
the Q4 measurement pass + manual browser verify remain (they need live
sessions). All automated gates green; implementation notes (18/10-byte
messages, decoder-output measurement point, cached-keyframe-style mapping
lifecycle) are in the doc. The audit there found most of the sketch above already landed
elsewhere: latest-frame-first rendering became R10 P1
(`CoalescingRenderSink`), and backlog discard + skip-ahead became the R8 S5
reorder buffer's drop-to-keyframe resync. The **keyframe-request
back-channel is rejected for good** (500 ms GOP, reliable keyframe streams,
and the R10 finding that keyframe *delivery* — not cadence — is the
bottleneck; a request signal makes the congested case worse). What remains,
designed as chunks Q1–Q4: a zero-protocol-change **live-edge drift** metric
(windowed-min baseline over the capture timestamps, which `capture.ts`
already stamps on the broadcaster's `performance.now()` clock), **absolute
glass-to-glass latency** via new `TimeSync` (0x05) / `ClockMapping` (0x06)
wire messages using the relay's monotonic clock as the common reference, an
**opt-in smoothed-playout mode** (reorder-release pacing,
`PLAYOUT_OFFSET_MS = 150`, default off, visibly costed — resolving the
trade R8 Decision 7 deferred here), and a measurement-driven tuning pass
over the GOP / `KEYFRAME_WAIT_MS` knobs.

## R6 — Production UI

**Goal**: the UI stops looking like a diagnostics rig. For the real
audience: fullscreen-first viewing, join by code or link, minimal chrome
that gets out of the way — think Netflix but sleeker, with Apple-calibre
polish and restraint. A friend clicking a join link should see the stream
and essentially nothing else.

**Why last**: the UI presents whatever feature set exists — join-by-code
(R1), broadcaster settings (R3), degradation notices (R4), live-edge status
(R5). Building it earlier means rebuilding it.

**Scope sketch**:

- **Viewer experience**: land on `#/view/<id>` → stream fills the viewport,
  controls (fullscreen, leave, maybe volume when audio ever lands) fade out
  after inactivity. Sensible idle/connecting/broadcaster-away states instead
  of raw status text.
- **Broadcaster experience**: start broadcast → pick screen → get shown the
  broadcast code + copy-link button prominently; settings (R3 pickers,
  broadcast secret) tucked behind an unobtrusive panel.
- **The current UI survives as the debug surface**: today's stats-heavy
  broadcast/view pages and the loopback page move behind a hidden `#/debug`
  route (not linked from the production UI) and stay maintained — they are
  the troubleshooting story. Additionally a lightweight stats overlay
  (keyboard shortcut, à la Netflix's `Ctrl+Shift+Alt+D`) in the production
  viewer covers the "is it the stream or my machine" question without
  leaving the sleek UI.
- Visual design pass: typography, spacing, dark-first palette, motion — this
  is a design effort, not just a re-layout, and the design doc should include
  actual mockups before implementation.

**Key design questions**: how much of the existing page code is reused vs.
rebuilt (the transport/media modules are UI-agnostic and carry over
untouched; the React pages are probably rebuilt); whether `#/debug` shares
components with the production UI or stays frozen as-is.

**Status**: done — implemented 2026-07-13, manual browser verify passed
2026-07-14 (chunks J1–J6) — see
[`docs/10-production-ui.md`](docs/10-production-ui.md). Taste locked
(monochrome/restrained, segmented code entry, preview-hero broadcaster,
subtle motion); the open key design questions are resolved in the doc (debug
stays frozen and does **not** share components; the React page shells were
rebuilt while all transport/media/state modules carry over untouched). All
automated gates green (tsc, 162 tests, lint, build); **manual browser verify
passed 2026-07-14** (visual polish + real WebTransport/WebCodecs/fullscreen/
screen share). Zero
server/wire/pipeline changes. Note R5 was skipped for now — the doc does not
depend on it and slots an R5 live-edge metric into the stats overlay if/when
it lands.

## R7 — Hardware-supported controls & capture constraints

**Goal**: Adjust the broadcaster resolution and framerate pickers in the UI to only show options that are verified to be supported by the browser's hardware acceleration capability, and apply selected settings directly to the capture layer (`getDisplayMedia` / `applyConstraints`) rather than just capping at the encoder/preprocessor level.

**Why here**: Prevents broadcasters from selecting unsupported high-framerate/high-resolution configurations that would force a fallback to software encoding or trigger hard caps mid-stream, and minimizes resource consumption by aligning capture parameters with the selected output target.

**Scope sketch**:

- **Hardware-Aware UI Controls**: Probe the browser's `VideoEncoder.isConfigSupported()` on startup or picker load, and dynamically disable or filter out picker options (e.g., 60fps at 4K) if they are resolved to software acceleration or are completely unsupported by the GPU.
- **Direct Capture Constraint Propagation**: When the user selects a resolution or framerate rung (or when auto fallback chooses one), propagate the constraints directly to the active `MediaStreamTrack` using `track.applyConstraints()`. This ensures the browser only captures what we actually intend to encode, saving capture pipeline overhead.
- **Dynamic Capture Resolution Capping**: Translate resolution rungs to specific capture constraints on `getDisplayMedia` and track updates, rather than capturing at native resolution and scaling via `FramePreprocessor` when scaled down.

**Key design questions**:
- How to handle `applyConstraints` failures or latency (e.g., how the pipeline is kept stable during live track renegotiation).
- How the "auto" fallback controller interacts with capture-level renegotiation without triggering infinite loop resets.
- Designing the UI representation for disabled options (e.g., warning tooltips explaining GPU hardware limits).

**Status**: not started.

## R8 — Worker Offloading & Reliable Keyframes

**Goal**: Maximize UI responsiveness by moving the viewer pipeline off the main thread, and prevent stream freezes by sending keyframes over reliable WebTransport streams instead of unreliable datagrams.

**Why here**: Addresses the two major performance bottlenecks discovered during manual verification and cross-browser testing—network drops leading to `Dropped Incomplete` (especially in Chrome), and decoder performance limiting frame rates (resulting in `Awaiting keyframe` drops). Moving to a Web Worker allows rendering with `OffscreenCanvas` for fluid UI, and reliable streams ensure that massive keyframes are never ruined by a single UDP packet loss.

**Scope sketch**:

- **Web Worker Pipeline**: Move `ViewerPipeline` logic, `WebTransport` reads, and WebCodecs decoding to a dedicated worker. Pass an `OffscreenCanvas` from the main thread so all rendering happens locally inside the worker.
- **Reliable Keyframe Transmission**: Broadcaster detects when a chunk is a keyframe, and rather than splitting it into 1100-byte UDP datagrams via `packetizeFrame`, encodes it and sends it over a `createUnidirectionalStream()`.
- **Go Server Backend Upgrade**: The server's `transport/server.go` and `hub` will need to be refactored to support accepting Unidirectional Streams from the publisher and multiplexing/forwarding them to all subscribers, handling backpressure appropriately.
- **Jitter/Reorder Playout Buffer**: Delta frames over datagrams may arrive before the reliable keyframe stream finishes. The viewer must buffer these "early" frames for up to 200ms and use their `timestampUs` to pace release into the decoder, smoothing out network micro-bursts.

**Key design questions**: How to manage server-side backpressure when a subscriber stalls on a reliable stream, preventing it from blocking the publisher's broadcast stream.

**Status**: **done (S1–S7: reliable keyframes + worker offload); browser-verified
2026-07-14** — full design in
[`docs/12-worker-and-reliable-keyframes.md`](docs/12-worker-and-reliable-keyframes.md)
(chunks S1–S7). Implemented end-to-end, covered by automated tests, and
confirmed working in the browser: keyframes now travel over per-subscriber
reliable uni streams with server store-and-forward fan-out (write deadline,
supersede-stale, late-joiner priming), and the viewer merges them with datagram
deltas in a pure, bounded reorder buffer with freeze-on-gap. **S6 (worker
offload) is implemented and verified**: the whole viewer pipeline (and reconnect
state machine, via a reused `ViewerSession`) runs in a Web Worker that renders
decoded frames to a transferred `OffscreenCanvas` and never `postMessage`s a
frame; a capability handshake plus a main-thread `ViewerSession` fallback covers
browsers/environments without worker WebCodecs. `/statusz` and both stats
surfaces expose the new keyframe-stream and reorder counters; all automated gates
are green and the end-to-end browser verification passed on the target build.
The design resolved the open questions: the relay uses
**store-and-forward fan-out** — it receives each keyframe stream *completely*
into a bounded, cache-doubling buffer (capped by a new `MaxKeyframeBytes`)
before opening one uni stream per subscriber, so the publisher ingest is
structurally decoupled from every subscriber; a subscriber that stalls is
`CancelWrite`-ed after a write deadline, superseded by the newest keyframe
(≤1 in-flight per subscriber), and recovers at the next keyframe — the datagram
"drops over stalls" discipline at stream granularity. **Only keyframes go
reliable; deltas stay on datagrams.** The preliminary sketch's fixed 200 ms
playout offset was **rejected** as contrary to the project's live-edge
philosophy — the viewer gets a bounded *reorder* buffer (no constant latency),
and constant-offset playout is left to R5. The worker offload keeps a
host-agnostic, synchronously-testable pipeline core and transfers the
`OffscreenCanvas` exactly once (reconnect logic moves into the worker). New
relay knobs are plumbed through `registryOptions` to production per the R2
finding.

## R9 — Observability & metrics

**Goal**: turn the accreted per-feature counters into a designed
observability system. The relay exposes **Prometheus-compatible metrics on a
non-public endpoint** (scraped in-cluster via a ServiceMonitor); both the
broadcaster and the viewer get a production stats overlay (the viewer's
interaction model, extended to both surfaces); and the metrics are chosen so
any playback problem can be attributed from data alone: broadcaster encode,
broadcaster→relay network, relay egress/bandwidth cap, relay→viewer network,
viewer decode, or viewer render.

**Why now**: R8 closed the last known transport-level failure mode, so the
remaining work is tuning and diagnosis — which today runs on vibes. The
relay's `/statusz` is served over HTTP/3 only (the server has no TCP
listener), so *nothing* can scrape or even conveniently `curl` it; the relay
has no ingress-loss signal (it can't distinguish "broadcaster never sent it"
from "uplink lost it"); neither client measures its own connection; and the
broadcaster's production UI shows almost no stats at all.

**Scope sketch** (full design in [`docs/13-observability.md`](docs/13-observability.md)):

- **Server**: a second plain-TCP ops listener (`-metrics-addr`, default
  `:2112`) serving `/metrics` (client_golang), `/healthz`, and a mirrored
  `/statusz`; a snapshot `prometheus.Collector` over the existing
  `Registry.Stats()` (one source of truth — no parallel bookkeeping);
  refined counters (keyframe-drop reasons split, `sendErrors` finally
  exposed, egress bytes); an RTP-style ingress-loss window (frames/chunks
  the broadcaster sent that never arrived — the leg-A attribution signal);
  route/limit outcome counters; per-subscriber detail in `/statusz`.
- **Chart**: metrics ClusterIP Service + gated ServiceMonitor; the metrics
  port never touches the public LoadBalancer.
- **Clients**: `WebTransport.getStats()` sampling (feature-detected;
  RTT/loss/send-rate per leg, per end) and a full pipeline **funnel** —
  capture → post-gate → encoded → *actually sent* fps on the broadcaster,
  received → decoded → rendered fps on the viewer; a shared sectioned
  overlay on both surfaces with windowed rates and a "Copy diagnostics"
  JSON export (the remote-troubleshooting story in place of any
  client-metrics-push channel).
- **Doc**: a bottleneck playbook mapping symptom → metric signature →
  verdict, kept in docs/13 for humans and Claude alike.

**Key design decisions** (resolved in the doc): separate TCP listener rather
than any attempt to scrape H3; snapshot collector over `Registry.Stats()`
rather than duplicate Prometheus counters in the hub; obfuscated-ID
`broadcast` labels with bounded cardinality and **no per-subscriber labels**
(per-subscriber detail lives in `/statusz` JSON); client→server metrics push
and true glass-to-glass latency are non-goals (the latter stays R5's,
slotting into the same overlay row later).

**Status**: done — implemented 2026-07-14 (chunks M1–M7), manually verified
2026-07-14; M8 — the Grafana dashboard and optional QUIC tracer — remains
deferred (needs dedicated time with the live homelab Prometheus/Grafana).
All automated gates green. Two
implementation notes fed back into the doc: the metric naming split into
`gawk_broadcast_*` (per-broadcast label) vs `gawk_relay_*` (lifetime totals)
because client_golang rejects one family with two label sets, and
`-metrics-addr` disables via the literal `off` (an empty env var reads as
unset). Manual verify — cluster scrape path, playbook attribution drills,
browser overlays on Chrome + Firefox — passed 2026-07-14; see the doc's
verification plan.

## R10 — Viewer render performance

**Goal**: close the Firefox viewer gap — a significant "Dropped (incomplete)"
rate plus decoded fps below received fps that Chrome doesn't show. Diagnosis:
the R8 worker runs transport, decode *and* render on one thread, and the sink
draws every decoded frame through Firefox's slow 2D-canvas
`drawImage(VideoFrame)` path (synchronous CPU conversion) — starving the
datagram reader (silent datagram drops) and the decoder simultaneously.

**Why now**: R9 made the funnel measurable, and the first real Firefox
sessions produced exactly the signature above. The two cheapest fixes both
live entirely behind the R8 `RenderSink` seam.

**Scope sketch**:

- **P1 — coalesced rendering**: latest-frame-wins, at most one draw per
  worker `requestAnimationFrame` tick; superseded frames closed unseen (the
  R5 "latest-frame-first rendering" bullet, landed early at the sink level).
- **P2 — WebGL render sink**: textured-quad `texImage2D(VideoFrame)` instead
  of 2D `drawImage` (chosen over `bitmaprenderer`, which adds a per-frame
  `createImageBitmap` allocation + async hop and may hit the same software
  conversion in Gecko); 2D sink kept as fallback; active renderer exposed in
  stats/overlay.
- **P3 — transport/decode worker split**: `ViewerPipeline` consumes a new
  `ViewerTransport` seam; the viewer worker spawns a **nested** transport
  worker (one per pipeline attempt) that runs the WebTransport read loops
  and posts datagrams/keyframes as transferable buffers, so decode/render
  pressure can never starve the browser's incoming-datagram queue. The same
  `LocalViewerTransport` implementation serves the in-process fallbacks.
- **P4 (partially pulled forward)** — decoder queue bound raised 5 → 10 (a
  briefly-behind decoder absorbs the burst instead of cycling
  drop-to-keyframe); the rest (decode-path confirmation, rung guidance)
  deferred pending measurements (Firefox H.264 WebCodecs is often software).

P1–P4 were zero server/wire/broadcaster changes; the main-thread fallback
path is untouched throughout.

**Status**: P1–P3 + decoder-queue bump implemented 2026-07-14; the first
field diagnostics session (same day, doc'd in docs/14) then produced two
follow-up fixes — `KEYFRAME_WAIT_MS` 200→1000 ms (store-and-forwarded
keyframes measured landing >500 ms behind their datagram deltas; 200 ms
degenerated every GOP into keyframe-only playback on a congested peer) and
**relay eviction of zombie subscribers** after 10 consecutive keyframe
stream-open failures (non-terminal close code 4001) — the one server-side
change in R10. Chrome 152 `getStats()` breakage was root-caused 2026-07-14:
Chromium removed the API entirely (see docs/13 D7), not a gawk bug.
Re-verification on both browsers passed 2026-07-14 (P4 remainder stays
deferred pending future measurements) — see
[`docs/14-viewer-render-performance.md`](docs/14-viewer-render-performance.md).

## R11 — Broadcaster worker offload

**Goal**: run the broadcaster's frame path — capture consumption, pre-encode
scaling/gating, encode, packetize, send — in a dedicated Web Worker, mirroring
the viewer's R8 S6 architecture, so main-thread work on the broadcast page
can never add jitter to the frame pipeline. Explicitly *not* an OS-priority
fix: a worker shares the renderer process, so Chrome's process-level
backgrounding applies regardless — this addresses main-thread *contention*
only (and the heavy lifting was already out of the renderer: NVENC runs in
the GPU process).

**Why now**: asked directly while investigating how to prioritize the
broadcaster against other apps on the gaming PC; chosen for architectural
symmetry with the viewer (S6) and as insurance against main-thread jank, with
the honest caveat above recorded in the design doc.

**Scope sketch** (full design in
[`docs/16-broadcaster-worker-offload.md`](docs/16-broadcaster-worker-offload.md)):

- **Track transfer, not readable transfer**: main thread runs
  `getDisplayMedia` (window scope + user gesture) and keeps the original
  track for the preview; a `track.clone()` is transferred into the worker,
  which creates its own `MediaStreamTrackProcessor` — frames are born,
  scaled, encoded and closed entirely in the worker, zero per-frame copies
  or postMessages.
- **`BroadcastPipeline` gains a media-source seam** (the R10 P3 move applied
  to the other end): the worker-side source waits for the transferred track;
  the default source wraps `startCapture` unchanged.
- **Connect-before-picker preserved**: the worker connects `/publish` first,
  then asks main for capture (`awaitingCapture`); `BroadcastStartError.phase`
  semantics (reclaim→mint only on `'connect'`) are identical.
- **Capability gate + fallback**: worker boot handshake (VideoEncoder,
  WebTransport, MSTP, OffscreenCanvas) plus a synchronous track-transfer
  probe (dummy `canvas.captureStream()` track; `DataCloneError` throws
  sender-side). Any failure → the untouched main-thread pipeline (Firefox
  always lands there). `BroadcastStats.pipelineContext` + an overlay
  "Pipeline" row make the placement observable.
- Zero server / wire / viewer changes; the frozen `#/debug/broadcast` page
  keeps the main-thread pipeline.

**Status**: implemented 2026-07-14 (chunks K1–K4; `BroadcastWorkerCore` +
`broadcaster.worker.ts` + `WorkerBroadcastSession`/`createBroadcastSession`,
seam refactor in `capture.ts`/`broadcaster.ts`). All automated gates green
(23 new tests). Manual browser verify pending — see the doc's verification
plan (Chrome worker path + funnel-rate baseline comparison, Firefox fallback,
main-thread CPU-throttle kill test).

---

## Explicitly out of scope (unchanged from CLAUDE.md)

Restated here so the roadmap doesn't quietly reopen settled decisions:

- **WebRTC + self-hosted SFU** (OvenMediaEngine, LiveKit, mediasoup) — the
  mature path, deliberately not taken; this project exists to explore
  WebTransport + WebCodecs.
- **Media over QUIC (MoQ)** — still an unstable IETF draft with no native
  browser support; a future direction, not a current target. Don't build
  toward it.
- **Persistent broadcaster channels** — deferred beyond R1 (see R1
  non-goals).
- **Viewer→server keyframe-request back-channel** — rejected for good in the
  R5 design ([docs/15](docs/15-viewer-live-edge.md), Decision 6): overtaken
  by the 500 ms GOP, reliable keyframe streams (R8), and cached-keyframe
  priming; the R10 field finding showed keyframe *delivery*, not cadence, is
  the bottleneck — a request signal produces more keyframes and makes the
  congested case worse, while breaking the one-way data-flow design.
