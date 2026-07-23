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
| R5 | [Viewer live-edge enhancements](#r5--viewer-live-edge-enhancements) | ✅ done (re-scoped; Q1–Q4) — manual verify passed 2026-07-15 ([docs/15](docs/15-viewer-live-edge.md)) |
| R6 | [Production UI](#r6--production-ui) | ✅ done (J1–J6); manual browser verify passed 2026-07-14 ([docs/10](docs/10-production-ui.md)) |
| R7 | [Hardware-supported controls & capture constraints](#r7--hardware-supported-controls--capture-constraints) | ⤳ superseded by R13 |
| R8 | [Worker Offloading & Reliable Keyframes](#r8--worker-offloading--reliable-keyframes) | ✅ done (S1–S7: reliable keyframes + worker offload); browser-verified 2026-07-14 ([docs/12](docs/12-worker-and-reliable-keyframes.md)) |
| R9 | [Observability & metrics](#r9--observability--metrics) | ✅ done (M1–M7); manually verified 2026-07-14; M8 (Grafana) still deferred ([docs/13](docs/13-observability.md)) |
| R10 | [Viewer render performance](#r10--viewer-render-performance) | ✅ done — P1–P3 + decoder-queue bump + field-finding fixes (keyframe wait 1 s, relay zombie eviction) implemented and re-verified on Chrome + Firefox 2026-07-14 (P4 remainder deferred) ([docs/14](docs/14-viewer-render-performance.md)) |
| R11 | [Broadcaster worker offload](#r11--broadcaster-worker-offload) | 🚧 implemented 2026-07-14 (K1–K4); automated gates green, manual browser verify pending ([docs/16](docs/16-broadcaster-worker-offload.md)) |
| R12 | [Viewer playback smoothing](#r12--viewer-playback-smoothing) | ✅ T1–T4 implemented 2026-07-15 (measurement + paced presentation + adaptive offset + interpolation scaffold); **adaptive + interpolation are the viewer defaults since 2026-07-15**; manual browser verify done 2026-07-19; T5 (motion-estimated interpolation) + T6 (findings) not started/droppable ([docs/17](docs/17-viewer-playback-smoothing.md)) |
| R13 | [Advanced broadcaster settings](#r13--advanced-broadcaster-settings) | 🚧 implemented 2026-07-15 (L1–L5); automated gates green, manual browser verify pending ([docs/18](docs/18-advanced-broadcaster-settings.md)) |
| R14 | [Native Linux broadcaster](#r14--native-linux-broadcaster) | ✅ V0–V7 implemented 2026-07-15, automated gates green; **manual verify on the gaming PC done 2026-07-19** (hardware encode/portal/GUI — not CI-reachable); V8 (direct Vulkan Video) still gated on V2's on-hardware result, not started ([docs/19](docs/19-linux-native-broadcaster.md)) |
| R15 | [System audio](#r15--system-audio) | 🔧 designed 2026-07-15, refreshed 2026-07-19 post-R16–R20; **N1–N6 implemented 2026-07-19; hardware playback 2026-07-20/07-21 produced six field findings, all fixed — incl. the Decision 10 inversion to video-master A/V sync, the Decision 12 reversal taking audio back off the R19 reliable carrier, and a live-edge audio buffer-depth floor. Automated gates green; hardware re-verification pending** ([docs/20](docs/20-system-audio.md)) |
| R16 | [iOS native fullscreen](#r16--ios-native-fullscreen) | ⚠️ U1–U3 implemented 2026-07-16; **U4 verdict 2026-07-19: native `webkitEnterFullscreen` still shows a black video on iPhone across three on-device passes → native tier not viable, pseudo-fullscreen (CSS) is the shipping path** (docs/21 U4 pre-registered verdict; BUGS.md) ([docs/21 U4 findings](docs/21-ios-video-fullscreen.md)) |
| R17 | [Relay scale-out & high availability](#r17--relay-scale-out--high-availability) | ✅ W1–W6 implemented 2026-07-16, automated gates green; kind two-pod smoke automated + **green in the `e2e-cluster` CI job (2026-07-18)**; remaining homelab drills (rollout/crash/rebind blips, conntrack empiricism) + 200-viewer scale proof closed as owner-accepted 2026-07-19 (CI non-goals — kind lacks the physics) ([docs/22](docs/22-relay-scale-out.md)) |
| R18 | [Live viewer count](#r18--live-viewer-count) | ✅ Y1–Y6 implemented 2026-07-18, automated gates green; cluster viewer-count check (origin `viewersGlobal` == Σ per-pod real viewers, edges excluded) **automated in the `e2e-cluster` CI job 2026-07-19**; single-pod browser/native + re-home + storm manual verify still pending ([docs/23](docs/23-live-viewer-count.md)) |
| R19 | [Resilient viewer mode for lossy networks](#r19--resilient-viewer-mode-for-lossy-networks) | ✅ X2–X5 implemented 2026-07-18, automated gates green; X1 netem/browser baseline + X6 verification done 2026-07-19 (lossy-network behaviour — not CI-reachable) ([docs/24](docs/24-viewer-network-resilience.md)) |
| R20 | [E2E testing in CI](#r20--e2e-testing-in-ci) | 🔧 Z1 done + Z2/Z3 implemented 2026-07-18; **both tiers green in real CI** (tier-1 `e2e` on every PR; `e2e-cluster` on the 2026-07-18 release PRs — Z3's green-on-a-release-PR acceptance met, origin/edge split + browser viewer asserted); Z5 browser-broadcaster implemented 2026-07-19 (spike: viable headless via tab capture — screen capture delivers black frames); Z4 burn-in → required flip pending ([docs/25](docs/25-e2e-testing-in-ci.md)) |

---

## R1 — Multi-broadcaster support

**Goal**: multiple simultaneous, independent 1-to-many sessions. Many
broadcasters can each stream to their own audience at the same time. Starting a broadcast
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
or an accidentally-shared URL can't take a deployment down.

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

**Non-goals**: real user accounts, tokens, or a login flow — out of scope for
a self-hosted deployment. DDoS-grade protection — this targets self-hosted
operators behind their own edge, not an open public platform.

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

**Status**: done — re-scoped + designed 2026-07-14, **Q1–Q3 implemented the
same day**, **Q4 measurement pass + manual verify passed 2026-07-15** (live
sessions; all knobs kept — `keyframeIntervalMs` 500, `KEYFRAME_WAIT_MS`
1000, `PLAYOUT_OFFSET_MS` 150. Measured glass-to-glass: ~50 ms with
hardware encode+decode — well under the 500 ms target — up to ~2500 ms when
software codec paths are involved; latency is dominated by codec path
placement, reinforcing R7) — see
[`docs/15-viewer-live-edge.md`](docs/15-viewer-live-edge.md).
All automated gates green; implementation notes (18/10-byte
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

**Status**: superseded by [R13 — Advanced broadcaster settings](#r13--advanced-broadcaster-settings)
(decided 2026-07-15). Both bullets — hardware-aware UI controls and
capture-constraint propagation — carry into R13; the third bullet
(requesting the rung's resolution directly in `getDisplayMedia`) was
rejected there in favor of a broad grant + live `applyConstraints`, which
never requires re-prompting the screen picker.

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

## R13 — Advanced broadcaster settings

**Goal**: rehaul the resolution/framerate settings into a
hardware-acceleration-aware system. Probe the encoder's capabilities up
front (`VideoEncoder.isConfigSupported()` matrix over resolution × framerate
× codec × acceleration) so the **default is the best configuration that
hardware acceleration supports**; give the broadcaster an advanced panel
(acceleration tri-state, resolution, framerate, bitrate override, codec
override) with probe-annotated options; and align the **capture** layer with
the selection via live `track.applyConstraints()` so a 720p30 session stops
paying full 4K@60 capture cost — without ever restarting the stream.

**Why now**: supersedes R7 and absorbs both of its bullets. The R4
verification on the gaming PC showed the capture side is the real cost
(source-limited 4K capture), and the R4 hardware-path finding (HW encoders
don't surface backpressure via `encodeQueueSize`) means picking a
HW-supported config *up front* matters more than reacting later.

**Locked decisions** (full design in
[`docs/18-advanced-broadcaster-settings.md`](docs/18-advanced-broadcaster-settings.md)):

- **HW-aware auto ceiling**: 'auto' stays the default selection; the probe
  matrix sets its ceiling to the highest rung that resolves hardware at the
  selected fps (1080p ceiling when nothing does — Firefox, software mode).
  R4 stepping below the ceiling is untouched and stays **encode-only**
  (up-probes need the higher-res source still flowing).
- **The framerate default is probe-driven too**: a new 'auto' framerate
  selection (default) resolves framerate-first — **60 fps when any rung
  probes hardware at 60**, else 30; never 'native'. This consciously
  revises build-order item 11's 30 fps fan-out default: with hardware
  encode confirmed, 60 is the default experience (the software path keeps
  30). 'auto' fps never runtime-steps — R4 stays resolution-only.
- **Tri-state acceleration control**: auto (prefer HW, fall back) /
  hardware only (refuses to run software — the "did NVENC engage" mode) /
  software only.
- **Capture alignment = broad `getDisplayMedia` grant + live
  `applyConstraints`** on the sticky target (explicit rung or auto
  ceiling): **no settings change ever requires a stream restart** or
  re-picking the screen. The preprocessor stays as the safety net — frames
  remain truth (docs/01). On the R11 worker path, constraints apply to the
  transferred clone worker-side (the design's main spike).
- Probe-driven picker annotation (badges, never removal — explicit software
  choices are honored; the old force-cap-to-30 heuristics are removed),
  bitrate override ([0.5, 50] Mbps), codec pin, and an overlay Encode row
  showing the *actual* codec + acceleration (runtime truth over probe
  truth).
- Zero server / wire / viewer changes.

**Status**: implemented 2026-07-15 (chunks L1–L5; probe matrix, encoder
tri-state + overrides, HW-aware auto ceiling + 'auto' fps, capture
alignment, annotated pickers + advanced settings panel + overlay rows). All
automated gates green; zero server changes. Manual browser verify pending —
see the docs/18 verification plan, including the applyConstraints spike
outcome on the real gaming PC (Chrome worker path) and Firefox.

---

## R12 — Viewer playback smoothing

**Goal**: reduce residual playback judder beyond R5's fixed-offset smoothing,
in three layers: (1) a **jitter measurement foundation** (presentation-cadence
error measured at the actual paint, arrival-jitter percentiles, decode
jitter — so acceptance criteria are numbers); (2) a new, separate opt-in
**"Paced playback (adaptive)"** mode — sub-frame-accurate presentation pacing
(a `PacedPresentationSink` holding ≤3 decoded frames to vsync-aligned target
times, subsuming `CoalescingRenderSink`) with a jitter-tracked adaptive
playout offset (clamp(p95 − min + headroom, 50–350 ms), asymmetric slew)
instead of the fixed 150 ms; and (3) **experimental frame interpolation**
(WebGL2 block-matching optical-flow-lite, 30 → 60 fps) with a pre-registered
kill criterion — a documented rejection is a valid completion.

**Why now**: the docs/15 non-goal ("sub-frame presentation pacing — worth
revisiting if Q4 measures visible judder with smoothing on") is deliberately
re-opened, measurement-first. The existing "Smooth playback" (fixed 150 ms)
toggle is preserved as-is; the new machinery is a separate viewer opt-in
(user decision 2026-07-15). FEC and keyframe-burst taming were surveyed in
this design round and not selected — candidate future items.

**Scope**: client-only, zero server/wire changes; live-edge stays the
default; every drop/resync policy fires unchanged in every mode. Chunks
T1–T6 (T4/T5 interpolation droppable as a unit). Full design in
[`docs/17-viewer-playback-smoothing.md`](docs/17-viewer-playback-smoothing.md).

**Status**: designed 2026-07-15; **T1–T4 implemented the same day**
(jitter measurement, `PacedPresentationSink` + three-mode playout with the
new "Paced playback (adaptive)" toggle, `PlayoutController` adaptive offset,
opportunistic blend-interpolation scaffold behind its own experimental
toggle). **Default flipped the same day (user decision)**: the production
viewer now defaults to adaptive paced playback + interpolation; the
right-click menu disables either, and a legacy explicit opt-out migrates to
live-edge (docs/17 Decision 8, as superseded). All automated gates green
(391 vitest tests, build, lint); manual browser verify done 2026-07-19 (the
doc's verification plan; the R20 `e2e` browser job also runs the adaptive
paced + interpolated pipeline green on every PR — rendered fps ≈ 2× received,
the α=0.5 mid-slots). T5 (motion-estimated interpolation, droppable) and
T6 (measurement findings + constant verdicts) not started.

---

## R14 — Native Linux broadcaster

**Goal**: a standalone native broadcaster for Linux with **hardware encode**
as a hard requirement, bypassing the browser — a Gio **GUI app** for normal
use, plus a CLI over the same engine for headless/debug use. Easy to use,
minimal dependencies: every runtime dependency is a stock distro package, and
the desktop's share picker appears on every start (the choice is never
persisted — reversed 2026-07-16). Speaks the existing
publisher protocol byte-for-byte: zero server, wire and viewer changes. The
browser broadcaster is untouched and stays the path for Windows/macOS — and
for Linux machines without a usable hardware encoder (there is deliberately
no software rung here).

**Why**: the browser cannot do hardware encode on Linux, and this is a
platform gap rather than a tuning problem. WebCodecs `VideoEncoder` hardware
encode ships on Windows/macOS/Android only — Linux gets hardware *decode*
only — and Chromium's VA-API doc disclaims Linux support outright. On NVIDIA
it is structurally impossible: Chromium's Linux encode path is VA-API only,
and `nvidia-vaapi-driver` is decode-only by design. So a Linux broadcaster
means software x264 (and the game fps that costs) unless it stops being a
browser.

**Scope sketch** (full design in
[`docs/19-linux-native-broadcaster.md`](docs/19-linux-native-broadcaster.md)):

- **One engine, two shells, own module** — `gawk-broadcast/` (a new top-level
  Go module) holds `internal/engine`: a `Session` (`Start`/`Stop`) +
  `Callbacks`, deliberately mirroring the TS
  `BroadcastSessionLike`/`BroadcastCallbacks` so the two broadcasters stay
  legible to each other. `cmd/gawk-broadcast` (CLI) and
  `cmd/gawk-broadcast-gui` are thin shells over it. The GUI requirement is
  what forces this shape: a `main` full of flags and globals would have to be
  dismantled to grow a window. The CLI isn't scaffolding — it proves the
  engine end-to-end (V4 gates the protocol *and* the latency bias) and stays
  as the headless path, the analogue of the frozen `#/debug/broadcast` page.
- **V0 promotes `internal/wire` → public `gawk-server/wire`** so the new
  module can import it (Go's `internal/` rule forbade a top-level module; the
  originally-planned same-module layout silently coupled relay CI to Gio's
  cgo/header stack and made broadcaster commits bump + auto-redeploy the
  relay — decided 2026-07-15). Own release-please component
  (`gawk-broadcast-vX.Y.Z`), own CI job with the Gio build deps; relay CI,
  image and homelab deploys become immune to broadcaster work. Wire reuse
  stays compile-time-coupled via the local `replace` + golden vectors.
- **The viewer already auto-detects Annex-B vs AVCC** (the `isAnnexB`
  start-code sniff in `viewer.ts`), so the engine emits raw Annex-B with
  empty extradata and never builds an avcC record — the nastiest interop
  risk is designed out before it exists.
- **Capture = Go-owned XDG ScreenCast portal + GStreamer subprocess.** The
  engine does the portal handshake itself (~250 lines of `godbus`): the
  desktop's own share picker, cursor embedded. **The picker appears on every
  start — the choice is never persisted (reversed 2026-07-16; no restore
  token is requested or stored), so Start, Resume and restarts all re-prompt.**
  Within a single Start, cascade retries reuse the in-memory grant (no
  re-prompt). Capture/encode runs in a `gst-launch-1.0` child
  (`pipewiresrc fd=…`), preserving crash isolation; every runtime dependency
  is a stock distro package. **`pipewiregrab` was rejected on review
  (2026-07-15): it is not in mainline FFmpeg** — an unmerged patchset carried
  downstream by Jami; mainline ffmpeg has no PipeWire input at all. Don't
  re-propose it without verifying it merged.
- **Hardware-only encode, Vulkan first**: `vulkanh264enc` → `nvh264enc` →
  `vah264enc` → **refusal pointing at the browser broadcaster** (no software
  rung — user decision 2026-07-15; the browser already covers software).
  Each candidate accepted only after a **real trial encode** (`videotestsrc`,
  never the portal), last-good encoder cached, the live start is the final
  probe. Friends publish from GPUs we can't survey, so all three backends are
  permanent infrastructure.
- **MPEG-TS over the pipe** (not raw Annex-B + AUD splitting): one PES = one
  AU via `payload_unit_start_indicator`, `h264parse config-interval=-1`
  guarantees in-band SPS/PPS at every IDR (load-bearing — the empty-extradata
  config means late-joiner priming only works with self-sufficient
  keyframes), and PES PTS comes free as the upgrade path if V4's 15 ms
  timestamp-bias gate fails (upgrade = *clock-anchored* PTS; unanchored PTS
  only stabilizes the bias).
- **Vulkan Video (`VK_KHR_video_encode_h264`) is the target encode API**, in
  two stages — it is the only encode API spanning RADV, ANV and NVIDIA with
  no `nvidia-vaapi-driver`-shaped asterisk. **Direct VAAPI is rejected**:
  Chromium's Linux backend is VA-API only, which is *exactly why* it can't
  encode on NVIDIA. Stage 1 (V2): `vulkanh264enc` leads the cascade, and the
  **reference bitstream is generated offline with mainline ffmpeg's
  `h264_vulkan` (≥7.1)** from a committed y4m fixture — no capture, no
  patches. Stage 2 (V8): direct Vulkan Video in Go, gated on Stage 1, with
  differential criteria of decodes-clean + PSNR-within-ε-of-reference +
  structural sanity (byte/frame identity is unachievable and not the bar).
- **GUI is Gio** (native Wayland, no XWayland, no webview; Fyne/Wails
  v3/GTK4 rejected — see doc). **The window is the app**: closing it ends
  the broadcast, no background state. No preview (you're looking at your own
  screen). **Notifications via `godbus` with urgency levels — failures are
  critical urgency**, because KDE's portal inhibits normal notifications
  *while screen casting* (verified; the original design's only mid-game
  signal was suppressed by the act of broadcasting). **No viewer count and
  no "first viewer joined" notification** — nothing on the wire tells a
  publisher about subscribers (browser parity; a `SubscriberCount` message
  is a possible future wire change, not an R14 smuggle-in). **Tray and
  global hotkeys stay deferred**, with the research recorded in the doc.
- **No ladder, no auto-fallback**: fixed rung — **1080p60**, 500 ms GOP —
  now coherent because the engine is hardware-only by construction (tracks
  R13's framerate-first rule). `SetLadder` is cut from the v1 surface
  entirely (a rung change restarts the child; cheap to add later, though a
  restart now costs a share dialog since the choice is never persisted). R4's `FallbackController` is
  deliberately not ported — its `encodeQueueSize` trigger is exactly the one
  R4's own hardware finding showed never fires on HW encode. No
  auto-reconnect; **Resume** applies the browser's existing
  reclaim→mint-only-on-`connect` rule, and the engine's `StartError` carries
  the same `Phase` distinction *plus the HTTP status* (webtransport-go
  exposes it — 401/404/409/429 become sentences, which the browser's opaque
  `WebTransportError` can't do).
- **Encoder invariants + uplink policy are explicit acceptance criteria**:
  no B-frames (decode order == presentation order is a protocol assumption),
  ≤1 frame encoder-internal latency per candidate, drop-only fps gating
  (never CFR conversion of damage-driven capture), ≤1 in-flight keyframe
  stream with supersede, drop-frame-remainder on datagram send failure, MTU
  re-chunk on `DatagramTooLargeError`.
- Not a container/chart/CI-deploy component: binaries you run on your own PC,
  outside the cluster deployment model.

**Status**: designed 2026-07-15; **revised 2026-07-15 after design review**
(capture vehicle → Go-owned portal + GStreamer subprocess, hardware-only
Vulkan-first cascade with browser refusal, own module + public wire, MPEG-TS
framing, critical-urgency notifications, viewer-count features dropped;
chunks now **V0–V7 + V8** — V0 module split, engine V1–V3, CLI shell V4, GUI
V5–V6, docs V7, V8 direct Vulkan Video encode gated on V2's Stage-1 result).

**V0–V7 implemented 2026-07-15**; automated gates green (both Go modules:
gofmt, `go vet`, `go test -race`; frontend untouched). The engine's
integration tests build and run the **real `gawk-server`** and publish a
committed H.264 fixture through it, attaching a real subscriber — a fake
relay would only test our belief about the relay, which is the belief most
worth doubting in a second implementation. **Manual verification on the
gaming PC is done (2026-07-19)** — the gate that matters, and one that is
**not CI-reachable**: everything about hardware encode, the portal picker
(shown every start — never persisted), the V4 latency-bias measurement and
the no-hardware refusal is unobservable from a WSL2 box or a hosted CI runner
with no GPU encode block and no desktop portal. **V8 is not started** and stays
hard-gated on V2's on-hardware Stage-1 Vulkan result. Three recorded
deviations from the doc (see docs/19): the announce read is detached, the
mpegts fixture is ffmpeg-generated rather than pipeline-captured, and the
integration tests run the real relay binary.

---

## R15 — System audio

**Goal**: viewers hear the broadcaster's game audio, in sync with the video
to within casual tolerance, without giving up live-edge: audio drops are
concealed as brief silence, never as growing delay. Ships as an
**experimental, default-off** feature — an "Enable audio (experimental)"
toggle in the broadcaster's advanced settings; the viewer surfaces audio
controls (mute/volume, overlay Audio section) only when audio is actually
received in the stream. Graduation to default-on is a later explicit
decision.

**Why now**: the last big missing piece of the actual watching experience —
R6 reserved the volume-control slot and docs/15 reserved the clock story for
it. The transport layer is finally quiet enough (R8/R10 closed the video
failure modes) that a third media lane rides existing mechanisms.

**Direction (settled 2026-07-15 after an options survey)**: **Opus via
WebCodecs over datagrams** — chosen over Opus-over-reliable-streams
(head-of-line stalls, new server fan-out mechanism), raw PCM datagrams
(1.5 Mbps/viewer for nothing Opus doesn't give), and MediaRecorder+MSE
(container latency). One Opus packet (48 kHz stereo, 128 kbps, 20 ms,
~320 B) per datagram — no chunking, no keyframes, no reliable streams.

**Scope sketch** (full design in [`docs/20-system-audio.md`](docs/20-system-audio.md)):

- **Wire + relay**: new `TypeAudioFrame` (0x07, 16 B header: own seq space +
  timestampUs on the shared broadcaster clock) and `TypeAudioConfig` (0x08),
  golden vectors Go↔TS; hub gains two dispatch cases + a `cachedAudioConfig`
  join-prime slot mirroring the ClockMapping lifecycle; config re-sent at
  1 Hz (no keyframe to anchor re-emits to). Audio never touches the video
  ingress-loss window or `framesRelayed`.
- **Broadcaster**: parallel audio lane in `BroadcastPipeline` (audio MSTP →
  shared-clock anchor → `AudioEncoder` → datagrams), audio processing off
  (game audio, not voice); no-audio-track (Firefox, unchecked picker box) is
  a graceful video-only state; worker path transfers the audio clone beside
  the video clone. The toggle applies on the next broadcast start — the one
  R13 live-apply exception, forced by `getDisplayMedia` (an audio track
  can't be conjured without re-prompting).
- **Viewer**: demux in the reassembler; `AudioDecoder` in the viewer worker;
  decoded `AudioData` transferred to a main-thread `AudioWorklet` ring
  buffer (`AudioContext` can't live in a worker — the first deliberate
  decoded-media worker→main crossing); gaps → silence, late → drop,
  live-edge discipline throughout; no Opus FEC/PLC in v1 (WebCodecs exposes
  no hook).
- **A/V sync ("good-enough", user decision; direction inverted 2026-07-20
  — docs/20 field finding 4)**: one capture clock for both media makes skew
  a subtraction; `avSkewMs` measured always (target median ≤ 60 ms, p95
  ≤ 120 ms). **Video is the master clock and is never rescheduled for
  audio**, in every playout mode: audio is the medium with slack (one Opus
  packet per datagram, no reassembly or keyframe wait, so it arrives
  materially earlier than video), so it waits. Because the AudioWorklet
  consumes exactly `sampleRate` samples/s at 1×, alignment is a *start-time*
  decision — audio's first chunk is held until the video presentation
  schedule says it is due — and residual clock drift is a *rate* problem,
  absorbed by a sub-audible ±0.4% playback trim. Frame interpolation is
  unaffected by construction. (The original design made audio the master in
  the R12 paced modes; that inverted after it froze video on first hardware
  playback.)

**Non-goals**: microphone/voice mixing, multiple audio tracks, audio in the
R14 native broadcaster (wire messages are ready; noted as an R14 follow-up),
FEC/PLC, DTX, audio-only mode.

**Status**: designed 2026-07-15 (chunks N1–N6); design refreshed
2026-07-19 against R16–R20 (docs/20 "Design refresh"); **N1–N6 implemented
2026-07-19** — wire 0x07/0x08 + relay dispatch/cache, broadcaster audio lane
behind the experimental toggle (main-thread + worker paths), viewer decode →
AudioWorklet sink with the live-edge ring-buffer policies, A/V skew, and both
overlays' Audio sections. **First hardware playback 2026-07-20 produced four
field findings** (docs/20), all fixed test-first: the toggle failed the whole
broadcast where no system-audio source could start; video froze wholesale
because the display target and the release gate ran off different clocks; the
jitter buffer's target was an overflow ceiling with no floor, so the sink
played at ~0 ms depth; and — owner decision — **Decision 10 inverted to
video-master**, audio aligned at start to the video schedule with a
sub-audible rate trim for drift. Automated gates green in all three modules.
**Hardware re-verification of all four is pending**; deviations recorded in
docs/20 "Implementation status".

---

## R16 — iOS native fullscreen

**Goal**: the viewer's fullscreen button works on iPhone. Today it is a
**silent no-op**: iPhone Safari (and therefore every iOS browser — all
WebKit) has no Element Fullscreen API, and the viewer paints to a canvas,
so there is nothing to call iPhone's only native fullscreen —
`HTMLVideoElement.webkitEnterFullscreen()` — on. **Hard requirement: on
every device that has Element Fullscreen (desktop, Android, iPad ≥ 16.4),
nothing changes** — no video element, no new worker messages, byte-identical
behavior.

**Why now**: found while diagnosing iOS viewer reports (2026-07-15); the
on-device confirmation that iOS Safari runs the full worker pipeline
(transport + rendering in workers) resolved the design's one blocking
unknown. WebTransport shipped on iOS in Safari 26.4 (2026-03), and since
`VideoTrackGenerator` shipped in Safari 18, every iPhone that can watch a
stream at all has the API this design needs — no capability tiers among
connectable iPhones.

**Scope sketch** (full design in
[`docs/21-ios-video-fullscreen.md`](docs/21-ios-video-fullscreen.md)):

- **Canvas tee, not a new render path**: the R12 paced/interpolating WebGL
  sinks keep painting the OffscreenCanvas exactly as today; a new
  `TeeRenderSink` decorator wraps the context sink and, once armed, wraps
  each **presented** frame (interpolated mid-frames included) as
  `new VideoFrame(canvas, {timestamp})` into a worker-side
  `VideoTrackGenerator` — so the smoothed output, not raw decode, is what
  fullscreen shows.
- The generator's `MediaStreamTrack` transfers to the main thread once and
  feeds a hidden, **pre-armed** `<video playsinline muted>` (armed at
  `watching` — `webkitEnterFullscreen` needs an in-gesture call on a video
  that already has media, so lazy arming risks a dead tap).
- **Tiered `useFullscreen`**: element fullscreen where it exists (today's
  behavior, untouched) → `webkitEnterFullscreen()` on the armed video →
  CSS pseudo-fullscreen as the universal fallback — a silent no-op stops
  being reachable on any device.
- Gate: absence of `Element.requestFullscreen` (an iPhone signature),
  checked once on the main thread; capability probe
  (`VideoTrackGenerator` + trial `VideoFrame`-from-canvas) before any
  protocol change, with a **pre-registered fallback**: if the probe fails
  on real iPhone hardware, the native path is dropped and pseudo-fullscreen
  ships (documented rejection = valid completion, R12 pattern).
- **New "Feature Gates" stats-overlay section** (R16 is its first
  consumer): gate-controlled features listed by **UpperCamelCase** name
  (TS string-literal union) with active state + detail;
  `ViewerStats.featureGates`. First and only gate for now:
  `NativeVideoFullscreen`. The section renders only where gates are
  reported (broadcaster overlay untouched); it appears on every *viewer*,
  including non-gated ones (`✗ — element fullscreen available`) — the one
  deliberate, overlay-only R16-visible change outside iPhone.
- Viewer-client only: zero server/wire/broadcaster changes; `#/debug/*`
  untouched; main-thread pipeline fallback gets the pseudo tier only.

**Non-goals**: custom UI inside native fullscreen (structurally impossible —
the system player UI is the UI); an iOS-reachable stats-overlay opener
(real gap, separate item); audio through the element (R15's business);
offering the video surface on non-gated devices.

**Status**: U1–U3 implemented 2026-07-16 (gate + tiered fullscreen +
Feature Gates section; worker tee + generator/track transfer; hidden video
surface + native-fullscreen wiring); automated gates green. **U4 verdict
2026-07-19: the native path does not work on iPhone** — `webkitEnterFullscreen`
enters but shows a **black video** across three on-device passes, and the
decoded-frame clone tee (no canvas readback) did not cure it. Per the
pre-registered U4 criteria (high Content sample + still black ⇒ the native
player can't present locally generated MediaStreams on this WebKit), the
native tier is rejected and **pseudo-fullscreen (CSS) is the shipping path**;
the runtime probe (`new VideoFrame(OffscreenCanvas)` in a worker) and gate
already fall back to it. See docs/21 "U4 findings" and BUGS.md.

---

## R17 — Relay scale-out & high availability

**Goal**: the relay runs as N homogeneous pods behind the existing UDP
LoadBalancer — any pod can ingest or serve any broadcast, one hot broadcast's
audience spreads across pods (design target: hundreds of concurrent
broadcasts, ~500–1k viewers on a hot one), and a **version rollout never
breaks a stream**: the broadcaster auto-resumes (same ID, same viewer URLs,
no `getDisplayMedia` re-prompt) and every viewer's worst artifact is a ≤1 s
freeze. Pod crashes recover automatically within a few seconds (best-effort,
explicitly looser than the rollout bound). Single-pod deployments keep
byte-identical behavior behind a `-cluster-mode` flag.

**Why now**: product prep — this is the load-bearing gap between a
single-node demo and a deployment real audiences can rely on. Today a relay restart
doesn't blip streams, it **orphans** them: `/publish/{id}` on an unknown ID
returns 404 (no re-claim path), so every deploy forces a new broadcast ID and
kills every viewer URL; the broadcaster has no auto-reconnect at all (terminal
error UI + capture re-prompt); the chart hard-pins `replicas: 1` +
`strategy: Recreate`; and clients of an abruptly killed pod hang until the
~30 s QUIC idle timeout because no `StatelessResetKey` is configured.

**Direction (settled 2026-07-16 after an options survey)**: a
**self-federating origin/edge cascade over the existing WebTransport wire
protocol** — the publisher's pod claims a per-broadcast Kubernetes Lease and
becomes that broadcast's *origin*; other pods *edge-pull* on demand via an
internal subscribe route and re-fan-out locally. Join-prime, store-and-forward
keyframes, and drop-newest queues all compose per hop, and cascade depth is
structurally ≤ 2. Chosen over a NATS-backplane design (runner-up: a new
stateful system to operate and a TCP hop in the datagram media path —
revisit-if triggers recorded in docs/22) and over per-broadcast sharding
(caps a hot broadcast at one pod — fails the audience requirement).

**Scope sketch** (full design in [`docs/22-relay-scale-out.md`](docs/22-relay-scale-out.md)):

- **Rollout resilience first (W1–W2, valuable at replicas:1)**: SIGTERM drain
  sends a new non-terminal close code **4002** to every session *while the
  pod is still Ready* (kube-proxy flushes UDP conntrack on endpoint removal,
  so "unready then linger" is the wrong order); a **shared QUIC
  `StatelessResetKey`** makes abrupt deaths detectable in ~1 RTT; clients
  reconnect with 0 ms delay on 4002 (≤250 ms on abrupt errors) instead of
  today's 1 s floor.
- **Restart-survivable broadcasts**: broadcaster auto-resume (capture +
  encoder kept alive, transport-only reconnect, forced keyframe on re-attach)
  plus **resume tokens** — HMAC over the broadcast ID, keyed by the fleet
  `resumeTokenKey` when set (it wins over the HKDF-from-publish-secret
  fallback, which every secret-holder can compute — PR #47 review),
  delivered in-band as new wire message **0x09**, required for every
  `/publish/{id}` claim including IDs unknown to the receiving pod (which
  now create the hub). With the explicit key this also closes today's
  graced-ID hijack hole.
- **Federation (W3–W5, dormant behind `-cluster-mode`)**: per-broadcast Lease
  origin registry (force-take fencing via an `originGeneration`, leaderless
  janitor GC, lease deletion = cluster-wide "broadcast ended");
  `/internal/subscribe/{id}` edge pull (PSK-gated, generation-fenced, dials
  the lease's pod IP only — never the Service VIP); per-hop ClockMapping
  rewrite via a Go port of the client TimeSync estimator (no cluster-wide
  clock); edge prime caches invalidated on upstream loss; an origin losing
  its lease self-demotes to edge (closing edge sessions with **4003**).
- **Fleet plumbing (W5–W6)**: shared `statsKey` so a broadcast keeps one
  obfuscated metrics identity across pods; origin/edge role labels + a
  separate edge-leg ingress-loss window; per-IP limiter trusted-CIDR bypass
  (MetalLB L2 + `externalTrafficPolicy: Cluster` SNAT would throttle rollout
  reconnect herds; `Local` is rejected — L2 would pin all traffic to the
  announcing node); chart flips to `replicas: 2` + RollingUpdate
  maxSurge 1 / maxUnavailable 0 + PDB + a drain-aware `/readyz`.

**Key design questions (answered in docs/22)**: why drain must be
close-first (UDP conntrack semantics), why no server-side resume epoch is
needed (frameID continuity — R10's machinery already covers both resume and
restart), how deep the cascade can get (structurally 2), what happens on NAT
rebind split-brain (lease force-take + self-demotion), and which R2 limits
stay per-pod vs become cluster-wide.

**Non-goals**: zero-blip rollouts (QUIC session handoff isn't implementable
on quic-go and ≤1 s doesn't need it), crash RTO under the rollout bound
(~2–10 s typical, ≤ ~15 s target), geo edges (the cascade extends there
later), HPA (manual replicas in v1), cross-pod `/statusz` aggregation,
multi-tenant auth, MoQ.

**Status**: designed 2026-07-16 (chunks W1–W6); W1–W6 implemented
2026-07-16 with all automated gates green. Rebased onto R14 2026-07-18,
which surfaced and fixed a real interop bug: the native engine read only
the "first" server uni stream, but accept order is not open order — it now
dispatches by wire type and persists the resume token (docs/22 finding 9).
The kind two-pod smoke is now automated and **green in the `e2e-cluster` CI
job** (2026-07-18, docs/25 Z3): the real chart with `replicas=2` +
`clusterMode` on a pinned kind cluster, with the origin/edge split asserted
from per-pod `/statusz` and a real browser viewer green against the cluster.
The remaining homelab drills (rollout / crash / rebind blip measurements,
conntrack empiricism) and the 200-viewer load proof are closed as
owner-accepted 2026-07-19 — CI non-goals, since kind has none of that
physics. Per-chunk status and implementation findings in
[docs/22](docs/22-relay-scale-out.md).

---

## R18 — Live viewer count

**Goal**: both the broadcaster and every viewer of a stream see how many people
are currently watching it — a live "N watching" figure that updates promptly as
viewers join and leave. The broadcaster's production UI finally fills the
viewer-count slot `docs/10` already reserved (it renders nothing today); each
viewer sees the same count.

**Why**: this is the `SubscriberCount` message deliberately deferred from R14
(Decision 18) and named as future work in `docs/19`, `docs/10`, `CLAUDE.md` and
this roadmap. The count itself already exists server-side (`len(b.subs)`,
surfaced in `/statusz` and the `gawk_broadcast_subscribers` metric); what's
missing is a wire path to push it to clients. R14 refused to smuggle it in
precisely because it's a shared change that benefits **both** broadcasters
(browser + native) — so it lands once, for both, as its own wire+relay item.

**Scope sketch**:

- **Wire**: a new relay-originated message type (small fixed size: version +
  type + count). Its type byte must be **coordinated with R15's reserved
  0x07/0x08 audio types and R17's 0x09 resume token** — allocate the next
  genuinely free byte at implementation time. Golden vectors added to all three mirrors
  (`wire_test.go`, `wire.test.ts`, `wirecheck_test.go`).
- **Relay — the one genuinely new piece**: a **relay→publisher push channel**.
  The relay is a one-way byte forwarder today (it only reads from the publisher
  session; `hub.Publisher` holds no session handle). The count source already
  exists at the two mutation points — `Registry.Subscribe` and
  `Subscriber.Close`/`evict`, both under `r.mu` with the fresh `len(b.subs)` in
  hand. Fan-out **to subscribers** reuses the `ClockMapping` template
  (`fanOutLocked` + a cached value primed to late joiners + cache-invalidate on
  new publisher session). Pushing **to the publisher** needs new plumbing: give
  the hub a sender onto the publisher's `webtransport.Session`.
  Debounce/coalesce join/leave churn so a reconnect storm can't spam either end.
- **Browser broadcaster**: intercept the message in the existing publisher read
  loop (`broadcaster.ts`, which today only catches TimeSync replies); add a
  `viewerCount` field to `BroadcastStats`; render it in the reserved production
  slot + a Delivery overlay row.
- **Native broadcaster (R14)**: same interception in `engine.go`
  `readDatagrams`; add the count to the engine `Stats` + a `Callbacks` field;
  surface it in the Gio GUI. Removes the two "deliberately absent" comments
  (`engine.go`, GUI `main.go`).
- **Viewer**: add a case to the reassembler `switch` (mirroring
  `pushClockMapping`) → new `onSubscriberCount` callback → `viewerCount` in
  `ViewerStats` → overlay row, with an optional prominent "N watching" badge in
  `ViewerScreen`.

**Key design questions** (for the design doc):

- Push mechanism to the publisher: periodic datagram (simplest; matches the
  TimeSync-reply precedent, lossy → pair with periodic re-emit + cache) vs a
  persistent server→publisher uni stream vs reusing the announce stream.
- Exactly what counts: subscribers only (the broadcaster's own preview is not a
  viewer); whether a connecting-but-not-yet-subscribed session is included.
- Debounce window / max update rate; whether to also emit the current value
  immediately on a new join and on a new publisher session.
- Whether to bundle a "first viewer joined" signal (R14 mentioned it alongside)
  or keep v1 to a bare count.
- Whether the viewer sees a production-prominent badge or an overlay-only row.

**Non-goals**: viewer identity/names or a presence list (just a number);
historical/peak analytics (that stays `/statusz` + Prometheus territory);
per-viewer adaptation.

**Status**: **implemented 2026-07-18 (Y1–Y6, same day as the design)**;
automated gates green across all three modules. The **2-pod kind cluster
viewer-count check is now automated in the `e2e-cluster` CI job**
(2026-07-19, `e2e/cluster-assert.sh`: origin `viewersGlobal` == Σ per-pod
real viewers, edge-pod viewers counted, edge sessions excluded); single-pod
browser + native passes, re-home, and storm manual verify remain pending (the
docs/23 verification plan). Anchored on the cluster-mode counting model
(only real viewers, counted once, summed across the origin/edge cascade —
edges are plumbing, never counted): edges report their local viewer count up
the existing internal-subscribe session, the origin aggregates and pushes the
global total to the broadcaster + fans it to all viewers, and the whole thing
reuses the R5 `ClockMapping` cache/prime/fan-out template + a new wire type
`0x0B TypeViewerCount`. Backlog item promoted from R14 Decision 18 — and its
deferred "first viewer joined" notification ships with it (native GUI,
critical urgency, derived client-side from the 0 → ≥1 transition). See
docs/23's "Implementation status" for the recorded deviations.

---

## R19 — Resilient viewer mode for lossy networks

*(Numbering: `docs/23` is R18 above (live viewer count, designed 2026-07-18) —
hence R19 and `docs/24`.)*

**Goal**: a viewer on a lossy network (LTE/5G mobile, hotel Wi-Fi) enables a
new opt-in **"Resilient mode (mobile networks)"** and gets smooth,
artifact-free video at 0.5–2 s behind live instead of the freeze-and-stutter
the default live-edge mode produces under packet loss. Everyone else —
default-mode viewers, both broadcasters, the relay's behavior for
non-resilient subscribers — stays byte-identical.

**Why**: today a single lost delta datagram means freeze-until-next-keyframe
(up to ~500 ms), which at mobile-typical bursty loss rates fires several
times a second; and the R12 adaptive playout offset is clamped at 350 ms —
sized for LAN jitter, not cellular spikes. Every defense so far assumes loss
is rare; mobile viewers need a mode where loss is *expected* and paid for
with latency instead of stutter.

**Scope sketch** (full design in
[`docs/24-viewer-network-resilience.md`](docs/24-viewer-network-resilience.md)):

- **Opt-in per-subscriber reliable delivery** (survey verdict over
  buffer-only, relay-cache+NACK, and FEC): the viewer subscribes with
  `?delivery=reliable` (publish-secret query-param precedent) and the relay
  delivers its delta datagrams as length-prefixed **verbatim records on
  per-GOP reliable uni "carrier" streams** — QUIC retransmission recovers
  loss for free, the relay stays a byte forwarder (no frame reassembly), and
  the viewer feeds records into its existing datagram pipeline unchanged.
  Keyframe streams, store-and-forward, supersede and the 4001 eviction are
  untouched. Carrier rotation at keyframe fan-out is the drop point —
  drops-over-stalls preserved at GOP granularity. This deliberately
  supersedes docs/12 Decision 1 ("deltas stay on datagrams — permanently")
  **for this opt-in mode only**; the default mode keeps the founding choice.
- **One new stream-kind discriminator byte + record framing** (golden
  vectors in all three wire mirrors; the byte is allocated at implementation
  time — 0x09 is R17's resume token, 0x07/0x08 are R15's), zero new
  datagram messages, zero broadcaster changes (the lossy leg fixed is
  relay→viewer).
- **Extended adaptive buffering**: a resilient `PlayoutController` profile —
  clamp **[150, 2000] ms**, seed 500 — plus wider reorder-buffer capacity
  and RTT-scale gap patience; the same adaptive formula, so a clean mobile
  link sits well under 1 s. Pacing/interpolation/tee machinery all unchanged
  by construction. Degrades gracefully against an old relay (buffer-only).
- **Manual toggle first** (own right-click toggle, persisted, default off;
  mode change = deliberate reconnect); auto-detection is a designed-but-
  deferred suggest-banner keyed off existing loss/jitter stats — never a
  silent flip.
- Observability: Delivery-mode overlay row + carrier counters, `/statusz`
  and Prometheus reliable-subscriber dimensions, a bottleneck-playbook row.
- **R17 interop**: reliable conversion happens at the pod serving the
  subscriber; origin→edge internal subscriptions stay datagram-based
  (in-cluster links are effectively lossless) and the `delivery` param is
  never propagated upstream.

**Non-goals**: FEC and NACK/ARQ (rejected in the survey), auto-switching in
v1, broadcaster-side anything, per-viewer quality adaptation, audio (R15 —
one-paragraph amendment there when it lands), DVR/rewind.

**Status**: designed 2026-07-18 (chunks X1–X6 with per-chunk acceptance
criteria); **X2–X5 implemented 2026-07-18** — wire type 0x0A
(`TypeReliableCarrier`), relay carrier drain + per-GOP rotation, viewer
stream-kind dispatch + resilient profile, the toggle, and full observability;
automated gates green on both Go modules and the app. X1 (netem baseline +
three-engine browser spike — needs real browsers/hardware; implemented ahead
of it as a recorded deviation, see docs/24 "Implementation status") and X6
(verification + tuning, incl. the real-phone LTE session) done 2026-07-19 —
the lossy-network behaviour this mode exists for is not CI-reachable (the R20
E2E runs a clean loopback link with zero loss and default datagram delivery).
User
decisions anchoring scope: reliable-streams + extended-buffer mechanism;
~2 s adaptive latency budget; manual toggle first.

---

## R20 — E2E testing in CI

**Goal**: a GitHub Actions pipeline that proves, before a release ships,
that streaming actually works end-to-end — a real browser viewer decodes
and renders real frames published through the real relay binary — in both
**single-pod and cluster-mode** relay configurations. A regression that
breaks streaming should fail checks on the release PR, not be discovered
after the homelab auto-deploys the release.

**Why**: every automated gate today stops below the browser. `go test
-race` runs real in-process WebTransport servers, and
`gawk-broadcast/internal/engine/relay_integration_test.go` even builds +
runs the actual `gawk-server` binary and publishes a committed H.264
fixture through the native engine — but nothing anywhere executes
WebCodecs or a browser's WebTransport stack (the app's vitest suite is
jsdom), which is exactly where field regressions keep surfacing: nearly
every recent roadmap item carries a "manual browser verify pending" tail.
Meanwhile deploys are automated cluster-side on release, so a broken
release reaches the homelab unattended. R17 additionally left the
kind/k3s two-pod smoke as a pending manual drill
([docs/22](docs/22-relay-scale-out.md) "Verification plan") — the cluster
tier here automates it. The repo being private makes CI minutes billable
(2,000 free/month), so the pipeline is designed to a minutes budget.

**Scope sketch** (full design in
[`docs/25-e2e-testing-in-ci.md`](docs/25-e2e-testing-in-ci.md)):

- **Tier 1 — single-pod browser E2E on every PR**: real relay with
  `-dev-cert`, real video published by a new **`gawk-pubsim`** CLI (the
  native engine replaying the committed H.264 fixture on loop — the
  `relay_integration_test.go` pattern as a standalone tool, which also
  makes the manual `gawk-loadgen` drills self-contained), and the
  **production viewer page in headless system Chromium** via
  playwright-core (the `gawk-app:verify` skill's recipes), TLS solved by
  `cert_hash_hex` → the app's existing `serverCertificateHashes` support.
- **Success signal = the viewer's own R9 diagnostics JSON** (captured via
  the `clipboard.writeText` stub precedent), with **flow-shaped
  assertions** sized for a contended 2-core runner — frames received /
  decoded / rendered, no sustained stalls, ≈0 loopback ingress loss —
  plus a composited-screenshot non-black check; never fps ceilings or
  latency numbers.
- **Tier 2 — cluster-mode E2E on release-please PRs only** (user
  decision; `workflow_dispatch` escape hatch): fresh pinned **kind**
  cluster per run, the real chart with `replicas=2` +
  `config.clusterMode=true` + NodePort over kind `extraPortMappings`
  UDP (`kubectl port-forward` is TCP-only), `gawk-pubsim` + ~12
  `gawk-loadgen` sessions to spread across both pods, and per-pod
  `/statusz`/metrics assertions proving the origin/edge split — the
  automated successor to docs/22's pending two-pod smoke. Release-please
  refreshes the release PR per main merge, so this still re-runs against
  each merged change while a release is pending.
- **Budget guardrails**: concurrency cancel-in-progress, hard
  `timeout-minutes` caps, no push-to-main runs (every change arrives via
  a PR and the release PR re-checks the aggregate), measured minutes
  recorded with a trigger-narrowing (never assertion-thinning) fallback.
- **Flake policy**: both tiers land advisory, flip to required after a
  clean burn-in; every flake is a root-caused findings entry.
- **Browser-broadcaster tier is a pre-registered droppable stretch**
  (Z5): `getDisplayMedia` automation spike (`--auto-select-desktop-
  capture-source` → Xvfb headful → injected `BroadcastMediaSource` hook)
  with kill criteria — a documented rejection is a valid completion.

**Non-goals**: replacing the hardware-bound manual verifies (gaming-PC
hardware encode, iPhone native fullscreen — no CI runner has that
hardware, or a GPU at all); performance/latency benchmarking (shared
runners are far too noisy — functional "does it stream" only); the
homelab-only drills (conntrack flush empiricism, ECMP/MetalLB behavior —
kind has none of that physics); the 200-viewer scale proof (stays a
manual drill); Firefox in v1 (deferred with a recorded revisit-if);
CI-driven deploys (locked decision: CI publishes, the cluster deploys
itself).

**Status**: Z1 done + Z2/Z3 implemented 2026-07-18, Z5 implemented
2026-07-19 (chunks Z1–Z5 with
per-chunk acceptance criteria — `Y` is R18's, claimed by the same-day
docs/23 design). The Z1 spike verdict: **hash-pinned WebTransport works in
headless Chrome as-is** (no SPKI flag, no Xvfb). Test-the-test passed
locally (publisher killed / wrong ID both go red), and the whole tier-2
flow was rehearsed on a local kind cluster — executing docs/22's pending
two-pod smoke (origin/edge split proven from per-pod `/statusz`, browser
viewer green against the cluster). **Both tiers are now green in real CI**:
tier-1 `e2e` on every PR, and `e2e-cluster` on the 2026-07-18 release PRs
(runs 29659639321 / 29659067892 — Z3's green-on-a-release-PR acceptance met;
step 18 asserted the origin/edge split, browser viewer green). The Z5 spike
verdict: **the browser broadcaster works headless too, via tab capture** —
`--auto-select-tab-capture-source-by-title` against a harness-owned
animated tab (headless *screen* capture grants but delivers black frames);
a second tier-1 step publishes from the production broadcaster surface with
the encode funnel asserted from its own diagnostics, ~17 s wall locally.
Pending: the
Z4 burn-in → required flip — plus a re-run on the new self-hosted `ioio-k8s`
runners, since the CI runner migration landed 2026-07-19 after those green
runs (which were on GitHub-hosted runners), and the Z5 step's first CI
runtime measurement. See
docs/25 "Implementation status & findings".
User decisions anchoring scope: cluster-mode
verification on release-please PRs only; free-tier CI minutes as a
design constraint, held by trigger scoping rather than assertion
thinning.

## R21 — Relay DVR ring buffer for resilient mode

**Goal**: make resilient mode ride out a multi-second connectivity stall
with **no freeze and no lost frames**, by giving each broadcast a short
(2–3 s, configurable) ring of itself on the relay and serving every
resilient subscriber from its own cursor into that ring. Pre-registered
success criterion: a viewer with a 3 s playout offset survives a **2 s
total blackout** with zero discarded frames, given ≥3× recovery
bandwidth.

**Why**: R19 shipped the viewer half of the trade — a deep adaptive
playout buffer — but not the relay half, so the mode only survives *loss
and jitter on a connected link*, not an actual outage. The 2026-07-22
paired capture (BUGS.md) showed why: the viewer's buffer is made of
*delay*, not of pre-fetched data, so during a stall it needs precisely
the frames captured during that stall — and the relay destroys exactly
those, first at the 500 ms `CarrierWriteTimeout` and then in docs/24
finding 17's dead-GOP purge. Both are right for live-edge delivery and
fatal for a mode whose premise is that late frames are fine. Deepening
the viewer buffer alone does not fix it: it *relocates* the freeze to
after the outage and adds permanent latency. The capacity was never
missing (the per-subscriber queue is already ~2.6 s); what is missing is
keyframes inside that window and any notion of where each subscriber is
in the stream.

**Scope sketch** (full design in
[`docs/26-relay-dvr-buffer.md`](docs/26-relay-dvr-buffer.md)):

- **One ring per broadcast, cursors per subscriber** — one copy of the
  bytes, N readers; also makes fan-out O(1) instead of O(subscribers).
- **The ring stores GOPs**, not loose datagrams: a cursor must be able to
  start, and the only decodable start is a keyframe. Replay of a GOP is
  byte-identical on the wire to a live one, so **zero wire changes** and
  almost no viewer changes.
- **Falling off the tail is the only frame loss the mode has** — the
  design converts "stalls over 500 ms lose frames" into "stalls over the
  ring window lose frames". It moves the cliff; it does not remove it.
- **The buffer must strictly exceed the stall**: covering a stall `S`
  with buffer `B` needs burst bandwidth `B/(B−S)` × bitrate, so 2 s of
  buffer cannot cover a 2 s stall at any bandwidth. Per-subscriber
  catch-up ceiling (default 2×) keeps one recovering viewer from taking
  the pod's whole egress budget.
- **Audio joins the ring** behind its own flag — a partial reversal of
  docs/20 field finding 5, gated and measured, because a video-only DVR
  fixes the picture and leaves the sound full of holes.
- Health becomes "is the cursor advancing?", not "is it at live?", or
  the existing eviction thresholds start killing healthy viewers.
- Mode off ⇒ byte-identical; the ring is allocated lazily and freed with
  the last DVR subscriber.

Chunks **DV1–DV6** (every single-letter prefix A–Z is taken). Designed
2026-07-23, not started.

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
