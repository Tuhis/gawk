# R5 — Viewer Live-Edge: Measurement + Opt-In Smoothing

Design doc for [ROADMAP R5](../ROADMAP.md#r5--viewer-live-edge-enhancements),
**re-scoped 2026-07-14**. R5 was written before R8/R9/R10 existed, and those
items consumed most of its original scope (the audit below). What remains —
and what this doc designs — is:

1. **Live-edge measurement**, phased: a zero-protocol-change **drift** metric
   first (Q1), then **absolute glass-to-glass latency** via a small time-sync
   protocol addition using the relay clock as the common reference (Q2).
   Sub-500 ms glass-to-glass is the project's headline success criterion and
   is currently unverifiable from data — R9 explicitly left this hole to R5.
2. **An opt-in de-jitter playout mode** (Q3) — the "latency-for-smoothness
   trade" R8 Decision 7 deferred here. Default stays live-edge; the mode is
   a viewer-side toggle that adds a fixed, visible delay.
3. **A measurement-driven tuning pass** (Q4) over the knobs that today rest
   on assumptions: the 500 ms GOP, `KEYFRAME_WAIT_MS` = 1000 ms, and the
   recovery-after-stall bound.

The **viewer→server keyframe-request back-channel** — R5's original "key
design question" — is **rejected for good** (Decision 6), not deferred.

## Background: the audit of R5-as-written

The original roadmap entry, bullet by bullet, against what has landed since:

| Original R5 scope | Where it went |
|---|---|
| Latest-frame-first rendering | ✅ **Done — R10 P1.** `CoalescingRenderSink`: latest-frame-wins, ≤1 draw per worker-rAF tick, superseded frames closed unseen. |
| Backlog discard + skip-ahead (drop-to-keyframe on slow decode / backlog) | ✅ **Done — R8 S5 + R10.** The reorder buffer centralizes freeze-on-gap; decoder-queue-deep triggers `requestResync()` (viewer.ts), and `jumpToKeyframe()` releases the *freshest* buffered keyframe, discarding everything below it. |
| Live-edge measurement (glass-to-glass in stats) | ❌ **Not done — the live core of this doc.** R9's non-goals: *"True glass-to-glass latency… That is R5's live-edge measurement work."* R6 and R9 both reserved the overlay row. |
| Recovery-time bound evaluation | 🔁 **Transformed.** When R5 was written the GOP was 2 s; it is now 500 ms, keyframes are reliable streams, and the R10 field finding showed keyframe *transit* (store-and-forward, ~236 KB) can exceed the GOP on a congested peer — so recovery is bounded by GOP + keyframe transit, not GOP alone. Evaluating that needs the metric first → folds into Q4. |
| Key design question: viewer→server keyframe-request signal | ❌ **Rejected for good** — Decision 6. |
| (Inherited) constant-offset de-jitter playout mode | ➕ **New scope** — R8 Decision 7 deferred it here as "the correct home for any latency-for-smoothness trade… which starts with measurement." Designed as an opt-in mode in Q3. |

One premise of the original entry was wrong in a useful direction: it said
"the wire format already carries capture timestamps" as if that made
measurement trivial. The timestamps are on the **broadcaster's clock** —
useless for cross-machine latency without a clock story (R9's reason for
deferring). But they are a *better* clock than the entry knew:
`capture.ts` **re-stamps every frame with the broadcaster's
`performance.now()` at capture** on both capture paths (MSTP re-stamp, and
the Firefox rVFC path via `presentationTime`, which is on the same
performance timeline). So `timestampUs` ≡ broadcaster capture wall-time on a
single well-defined monotonic clock. Q1 exploits that with zero protocol
changes; Q2 only has to relate two `performance.now()` timelines through the
relay.

## Decisions (locked)

1. **Measure before tuning, and phase the measurement: drift (Q1) before
   absolute (Q2).** The drift metric needs zero wire/server changes and
   already answers the operational question ("is this viewer falling behind
   live, and by how much?"). Absolute glass-to-glass needs a clock-sync
   protocol addition and answers the *project* question ("is it actually
   sub-500 ms?"). Both land in the same overlay section; Q1 ships value even
   if Q2 slips.

2. **Live-edge drift = arrival delta minus windowed minimum.** Per frame,
   the viewer computes `delta = viewerPerfUs − frame.timestampUs`. That
   delta is (unknown clock offset) + (true capture→here latency); its
   **minimum over a sliding window** is the session-best baseline, and
   `drift = delta − windowedMin ≥ 0` is pure accumulated lag — decoder
   backlog, reorder holds, queue growth — with the clock offset cancelled.
   - **Windowed** (~60 s of 1 s buckets), not session-min: consumer crystal
     oscillators skew tens of ppm (≈ 36–360 ms/hour), which would silently
     inflate a session-long baseline; a sliding window absorbs it. The
     estimator lives in a small pure module (`transport/live-edge.ts`,
     injected clock, node-testable) because Q3's pacing reuses it.
   - **Measured at decoder output on both paths** (amended at
     implementation from "render sink inner draw"): the pipeline's
     `handleDecoded` is the last point both the worker and main-thread paths
     share, keeping one code path and a per-pipeline tracker lifecycle with
     zero `RenderSink` API churn. The rAF-coalesced paint follows by at most
     one display interval — a near-constant the windowed-min baseline
     cancels for drift; for the absolute number (Q2) it is a ≤ ~17 ms
     understatement, far inside the clock-sync error budget.
   - **Baseline resets on broadcaster restart** — timestamps jump to a new
     timeline. The reorder buffer already detects restart (a serially-
     backwards keyframe); the same signal resets the estimator.
   - What drift deliberately does **not** see: constant components (encode
     time, base network transit, relay store-and-forward floor). Those are
     Q2's job.

3. **Absolute glass-to-glass uses the relay clock as the common reference**
   — a `TimeSync` ping/pong on each client's existing session plus a
   broadcaster-published `ClockMapping`, both new wire messages (Q2).
   - **`TimeSync` (0x05, datagram, 18 bytes)**: `version(1) + type(1) +
     clientTimeUs(8) + serverTimeUs(8)`, big-endian with the common 2-byte
     prefix like every wire message. A
     client sends it with `serverTimeUs = 0`; the relay echoes
     `clientTimeUs` and fills `serverTimeUs` from a **monotonic µs clock**
     (Go `time.Since(processStart)` — immune to NTP steps; it's a reference,
     not wall time). Client computes `rtt = t1 − t0` and
     `offsetUs = serverTimeUs − (t0 + rtt/2)` mapping its `performance.now()`
     timeline onto the relay clock. NTP-style filter: ping every ~2 s, keep
     the **lowest-RTT sample** in a rolling window (~8 samples) — asymmetry
     error is bounded by rtt/2 of the *best* sample, tens of ms on target
     networks vs. the 500 ms budget.
   - **`ClockMapping` (0x06, datagram, 10 bytes)**: `version(1) + type(1) +
     offsetUs(8, int64 two's complement)`, where
     `relayClockUs = frame.timestampUs + offsetUs`. The broadcaster computes
     it from its own `TimeSync` offset (its frames are already stamped on
     its performance timeline — see Background) and re-sends every ~5 s
     (skew refresh). The relay gives it **the cached-keyframe lifecycle**:
     the hub caches the latest mapping, fans live ones out to subscribers,
     primes every new subscriber with the cache, and invalidates it on a new
     publisher session. (The design sketch said "re-emit like
     `DecoderConfig` before every keyframe's chunk 0" — that pre-R8
     mechanism no longer exists; since R8 the config rides inside keyframe
     streams and the hub has no config re-emit path. Cache + join-prime is
     the current analogue and is what's implemented.)
   - Viewer math, at the same measurement points as Decision 2:
     `g2gMs = ((viewerPerfUs + viewerOffsetUs) − (frame.timestampUs +
     broadcastOffsetUs)) / 1000`. Total error ≈ sum of two best-sample
     rtt/2 asymmetries; a negative result (asymmetry pathology, not time
     travel) is clamped to 0 — the clamp itself, alongside the two RTT rows,
     is the flag.
   - **Replies must not queue behind video.** The relay answers `TimeSync`
     inline from the session's read loop via a direct `SendDatagram` — never
     through the per-subscriber video queue and never dropped by the egress
     bandwidth accounting (17 bytes/2 s is noise; delaying it destroys the
     measurement). Defensive rate limit: at most ~5 replies/s per session,
     excess silently dropped — a constant, not a knob (correctness-scale,
     like the eviction threshold).
   - **Rejected alternatives**: wall clocks (`Date.now()` both ends) —
     friends' machines have no NTP guarantee, 100 ms+ errors are routine;
     `WebTransport.getStats()` RTT — no browser ships it today (Chromium
     removed its pre-spec implementation in 152; docs/13 D7) and provides no shared clock anyway; relay-stamped frames —
     measures only the relay→viewer leg and turns the byte-forwarder into a
     rewriter. Side benefit of the ping: a **self-owned per-leg RTT** that
     doesn't depend on `getStats()`, shown in the overlay's Network section.

4. **Opt-in smoothing paces *release into the decoder*, not held decoded
   frames** (Q3). A paced frame is released from the reorder buffer when
   `now ≥ timestampMs + baselineOffsetMs + PLAYOUT_OFFSET_MS`, where
   `baselineOffsetMs` is Q1's windowed-min baseline — i.e. a constant offset
   from the source clock, anchored by the estimator we already built.
   - **Why pre-decode**: holding decoded `VideoFrame`s at the sink would be
     the "true" presentation pacer, but decoder output frames come from a
     bounded frame pool — holding ~9 of them (150 ms @ 60 fps) risks
     stalling the decoder outright. Decode latency variance on the happy
     path is milliseconds, so pacing the decoder's *input* delivers the
     smoothness where it matters (network jitter, the two-channel race)
     without touching frame lifetimes. rAF coalescing then presents at
     display cadence as today.
   - **Default OFF, and visibly costed.** The toggle ("Smooth playback")
     lives in the production viewer's existing right-click menu, persists in
     local storage, crosses to the worker as a control message, and can
     change live (the buffer re-paces). The overlay shows
     `Playout: live-edge` / `smoothed (+150 ms)`, and the Q1 drift row
     inflates by the offset — the cost is on screen, per the project's
     philosophy that this trade must never be silent.
   - **`PLAYOUT_OFFSET_MS = 150`** — a named tunable next to
     `KEYFRAME_WAIT_MS`, one value, no rung ladder until someone asks. 9
     frames at 60 fps, comfortably inside `MAX_BUFFERED_FRAMES` (64).
   - **Smoothing adds delay, not patience.** Every drop/resync policy fires
     unchanged in smooth mode: gap grace, keyframe wait, decoder-queue-deep
     resync, the frame cap. The mode delays *decodable* frames; it never
     waits longer for missing ones. `tick()` gains a real scheduling
     obligation (a paced frame can become due with no new arrivals), so the
     pipeline arms a short timer when paced frames are pending — today's
     tick-on-events cadence is not enough by itself.

5. **Where the numbers surface** — the reserved slots, nothing new: the
   viewer stats overlay's **Delivery** section gains `Live-edge drift` (Q1)
   and `Latency (capture→render)` (Q2) rows plus the `Playout` mode row
   (Q3), next to the stall-age rows they complement; the Network section
   gains the self-owned `RTT (time-sync)` (Q2) on **both** surfaces
   (broadcaster too — `BroadcastStats.timeSyncRttMs`). All of it flows
   through `ViewerStats` → worker outbound → Copy diagnostics JSON, exactly
   like R9's fields. No Prometheus export — client metrics stay client-side
   (R9's client-push non-goal stands); the copy-diagnostics button remains
   the remote-troubleshooting story.

6. **The keyframe-request back-channel is rejected for good.** Overtaken
   three times since it was posed: the GOP fell 2 s → 500 ms (worst-case
   wait for a natural resync point is ≤ 0.5 s), keyframes became reliable
   streams (R8 — the "my keyframe got lost" case no longer exists), and the
   relay primes joiners/resyncers from the cached keyframe. Decisively: the
   R10 field finding showed the real keyframe bottleneck is *delivery*
   (store-and-forward transit of ~236 KB), not *cadence* — a request signal
   produces **more** keyframes and makes the congested case strictly worse.
   It would also break the one-way data-flow design (relay never talks back
   to the broadcaster) for negative benefit. Moves to the explicitly-set-
   aside list; if a future design ever wants much longer GOPs, it can argue
   from this paragraph.

7. **Wire discipline as always**: golden test vectors for 0x05/0x06 keep Go
   (`wire_test.go`) and TS (`wire.test.ts`) byte-identical; both messages
   are strict-parsed (bad length → drop the datagram, never panic, per R2);
   any server knob (none planned — the rate limit and cadences are
   constants) would cross `registryOptions` per the R2 finding.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| Q1 | **Live-edge drift metric** — pure `transport/live-edge.ts` estimator (windowed-min baseline, injected clock); measured at decoder output on both paths (see Decision 2, amended); restart resets baseline; `ViewerStats.liveEdgeDriftMs` + overlay row + diagnostics JSON | Unit tests (fake clock): drift = delta − windowed min, min ages out of the window (skew absorbed), baseline resets on the restart signal, null before first frame; the measurement uses the decoded frame's capture timestamp; worker outbound forwards the field unchanged; overlay renders the row; **zero wire/server diffs** | ✅ done (2026-07-14) |
| Q2 | **Absolute glass-to-glass** — `TimeSync` (0x05) ping/pong on publish + subscribe sessions (monotonic relay clock, inline reply, ~5/s per-session cap); `ClockMapping` (0x06) broadcast→relay, cached + join-primed like the cached keyframe, invalidated with it; client min-RTT offset estimator; `ViewerStats.capToRenderMs` + self-owned RTT on both surfaces; overlay + diagnostics rows | Golden vectors 0x05/0x06 byte-identical Go↔TS; strict parse drops malformed input; server replies on both routes (e2e session tests), replies sent inline from the read loop (never the video queue), flood capped by the reply limiter; hub caches/fans out/join-primes/invalidates the mapping (hub unit tests); estimator converges on skewed fake clocks and prefers the lowest-RTT sample; negative g2g clamps to 0; the viewer subtraction is pinned end-to-end (fake transport + decoded frame → 250 ms); overlay renders latency + RTT; all gates green | ✅ done (2026-07-14) |
| Q3 | **Opt-in smoothed playout** — reorder-buffer pacing (`PLAYOUT_OFFSET_MS = 150` in `transport/playout.ts`, arrival-baseline anchored; off by default); due frames release from the existing 16 ms reorder tick; right-click-menu toggle, persisted (localStorage), live-switchable, plumbed to the worker via a `playout` command; `Playout` overlay row from the pipeline's context (ground truth) | Unit tests (fake clock): paced release at schedule, offset 0 releases immediately; toggling mid-session re-paces both ways; gap/keyframe-wait/queue-deep policies fire identically under pacing; a due paced frame releases via tick with no new arrivals; UI toggle persists, applies on mount, and sets the playout module (main-thread path); overlay shows the mode; default off | ✅ done (2026-07-14) |
| Q4 | **Measurement-driven tuning pass** — with Q1+Q2 live, evaluate: steady-state g2g vs. the 500 ms target (Chrome + Firefox, LAN + remote peer), keyframe transit vs. `KEYFRAME_WAIT_MS` (1000 ms), GOP (500 ms) vs. recovery-after-stall, smoothed-mode judder delta | A "Measurement findings" section in this doc with the numbers from both browsers incl. one remote peer; explicit keep/change verdicts recorded for `keyframeIntervalMs`, `KEYFRAME_WAIT_MS`, `PLAYOUT_OFFSET_MS` (changes land test-first as usual); ROADMAP status synced | ✅ done (2026-07-15 — ran with the manual verification below; all knobs kept, see "Manual verification findings") |

Q1 → Q2 → Q3 → Q4 is the dependency-honest order (Q3 reuses Q1's baseline;
Q4 needs Q1+Q2), but Q3 does not depend on Q2 and may land either side of it.

## Implementation notes (2026-07-14)

Q1–Q3 implemented test-first; all automated gates green (Go: gofmt/vet/`go
test -race`; app: 300 vitest tests, `tsc -b` via build, lint; helm lint).
Deviations from the design sketch, each folded back into the decisions above:

- **Message sizes are 18/10 bytes**, not 17/9 — every wire message carries
  the common 2-byte version+type prefix; the sketch forgot the version byte.
  Both are strict-parsed (exact length).
- **Drift + latency are measured at decoder output on both paths** rather
  than at the render sink's inner draw (Decision 2, amended in place) — one
  shared code path, no `RenderSink` API change; the paint follows within one
  display interval.
- **ClockMapping gets the cached-keyframe lifecycle**, not the pre-R8
  config re-emit (Decision 3, amended in place): hub cache + live fan-out +
  join prime, invalidated in `StartPublish`.
- **The TimeSync reply limiter is transport-level** (`server.go`): a tiny
  per-session token bucket (5/s, burst 5), answered inline from each
  session's read loop via `SendDatagram` — structurally incapable of
  queueing behind video. Constants, not knobs; **zero new config surface**,
  so nothing to plumb through `registryOptions`/Helm this time.
- **The overlay rows live in the Delivery section** (drift, latency,
  playout — next to the stall ages they complement), with `RTT (time-sync)`
  in Network on both surfaces.
- **Q3's worker crossing is a `playout` command** setting module state in
  the pipeline's JS context, read live by the reorder buffer each advance —
  so a mid-session toggle re-paces without touching pipeline lifecycles,
  and the overlay's `Playout` row reports from the same context (a toggle
  that failed to cross is visible).
- **Version-skew posture verified in tests**: against a relay that never
  answers TimeSync (older server), drift keeps working, the latency/RTT
  rows read `—`, and nothing throws.

## Verification plan (manual, both browsers)

- **Drift sanity (Q1)**: steady playback on a healthy LAN viewer shows
  drift hovering near 0; artificially load the viewer (CPU throttle in
  devtools) and watch drift climb, then snap back after a resync — the
  metric must move with the known R10 failure signatures.
- **Absolute sanity (Q2)**: cross-check `Latency (capture→render)` against
  the old-fashioned ground truth once — broadcaster shares a millisecond
  clock page, photograph both screens; agreement within ~50 ms validates
  the clock chain. Verify a late joiner gets a mapping (latency row
  populates within one keyframe) and that a broadcaster restart re-converges.
- **The headline number**: with a Chrome broadcaster on the gaming PC and a
  remote viewer, is glass-to-glass under 500 ms? This is the project's
  success criterion, measured from data for the first time — record it in
  Q4's findings either way.
- **Smoothing (Q3)**: on a jittery link (or simulated), toggle smooth
  playback live — judder visibly decreases, the latency row rises by
  ~150 ms, toggling off returns to live-edge without a stall or reconnect.
- **No-regression**: overlays on both surfaces still render with the relay
  *not* answering TimeSync (older server): drift works, latency row shows
  `—`, nothing throws — version skew between app and relay is a deploy
  reality (release-please versions them independently).

## Manual verification findings (2026-07-15)

The manual verification pass (plan above) and the Q4 measurement pass ran
together on live sessions and **passed 2026-07-15**.

**Headline (glass-to-glass vs. the 500 ms target)**: steady-state
capture→render latency was observed between **~50 ms and ~2500 ms**,
depending on whether the broadcaster and viewer were on hardware-accelerated
codec paths. On the intended happy path (hardware encode *and* decode) the
sub-500 ms target is met with a wide margin; with software encode/decode in
the chain, latency can exceed the target by ~2 s. Latency attribution is
therefore dominated by **codec path placement, not transport** — which
strengthens the R7 case (surface/steer hardware-supported configurations)
rather than motivating any pipeline knob change here.

No knob changes fell out of the pass — the keep verdicts are:

| Knob | Value | Verdict |
|------|-------|---------|
| `keyframeIntervalMs` (GOP) | 500 ms | keep |
| `KEYFRAME_WAIT_MS` (reorder keyframe wait) | 1000 ms | keep |
| `PLAYOUT_OFFSET_MS` (smoothed-playout offset) | 150 ms | keep |

The remaining per-browser detail numbers (keyframe transit vs.
`KEYFRAME_WAIT_MS`, recovery-after-stall vs. GOP, smoothed-mode judder
delta) were checked during the pass and judged OK but not recorded — if a
future live session captures exact figures, add them here.

## Non-goals

- **A keyframe-request back-channel** — rejected for good (Decision 6).
- **Changing the live-edge default.** Smoothed playout is opt-in, per
  viewer, visibly costed; the project's drops-over-stalls philosophy stands.
- **Per-viewer adaptation** (simulcast/SVC) — unchanged from R4/R10.
- **Client→server metrics push / Prometheus export of client latency** —
  R9's non-goal stands; Copy diagnostics carries the numbers.
- **Sub-frame-accurate presentation pacing** (holding decoded frames to
  vsync-aligned deadlines) — Decision 4 explains the frame-pool risk; only
  worth revisiting if Q4 measures visible judder *with* smoothing on.
  **Re-opened 2026-07-15 as R12**
  ([docs/17](17-viewer-playback-smoothing.md)): measurement-first, as a
  *separate* opt-in mode — this doc's fixed-offset "Smooth playback" stays
  as-is, and R12's decode-lead layering answers the frame-pool objection.
- **Audio/AV sync** — there is no audio; when audio lands, its clock story
  should build on Q2's relay-clock mapping.
