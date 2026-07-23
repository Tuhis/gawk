# R12 — Viewer Playback Smoothing: Paced Presentation + Adaptive Offset + Experimental Interpolation

Design doc for [ROADMAP R12](../ROADMAP.md#r12--viewer-playback-smoothing)
(designed 2026-07-15; **T1–T4 implemented the same day** — all automated
gates green, manual browser verify done 2026-07-19; T5/T6 not started — see
Status and Implementation notes). The R20 `e2e` browser job additionally
runs the adaptive paced + interpolated pipeline green on every PR (rendered
fps ≈ 2× received — the α=0.5 interpolation mid-slots), though smoothness
*quality* stays a manual/on-device judgement (a flow-shaped CI non-goal). This deliberately re-opens a
docs/15 non-goal: *"Sub-frame-accurate presentation pacing … only worth
revisiting if Q4 measures visible judder with smoothing on."* We are
revisiting it with the measurement foundation built in first (T1), so every
later chunk's acceptance criteria are numbers, not vibes. Three directions,
in dependency order:

1. **Jitter measurement** (T1) — presentation-cadence error measured at the
   actual paint, arrival-jitter percentiles, decode jitter. docs/15 Q4
   (passed 2026-07-15) judged the smoothed-mode judder delta OK by eye but
   recorded no figures; T1 is what makes that number recordable.
2. **Paced presentation + adaptive playout offset** (T2–T3) — a new,
   separate opt-in mode ("Paced playback (adaptive)") that holds *decoded*
   frames to vsync-aligned target times and sizes its delay from measured
   arrival jitter instead of a fixed constant. The existing "Smooth
   playback" (fixed 150 ms) toggle is preserved **byte-for-byte as-is**.
3. **Frame interpolation** (T4–T5) — an explicitly experimental,
   learning-motivated exploration of synthesizing intermediate frames in
   WebGL2 (30 → 60 fps), with a pre-registered kill criterion. A documented
   rejection is a valid completion.

Scope: **client-only — zero server/wire changes.** FEC parity chunks and
keyframe-burst taming were surveyed during this design (2026-07-15) and not
selected; they are candidate future roadmap items, not part of R12.

## Background: why judder still exists

Three quantization/jitter sources stack on a 30 fps stream (the default fps
cap since heavy-motion resilience) shown on a 60+ Hz display:

1. **Reorder release is 16 ms-granular and pre-decode.** Smoothed mode paces
   the *decoder's input* (`releasableAt()` in `transport/reorder-buffer.ts`,
   driven by the `REORDER_TICK_MS = 16` interval in `transport/viewer.ts`)
   to ±16 ms. Nothing paces the paint.
2. **`CoalescingRenderSink` has no notion of a target time**
   (`transport/render-sink.ts`, R10 P1): whichever frame is newest at the
   next rAF tick gets painted. A 33.3 ms source cadence sampled by a
   16.7 ms vsync with ±16 ms upstream jitter produces the classic
   1-vsync/3-vsync beat pattern *even when every frame arrives*.
3. **`PLAYOUT_OFFSET_MS = 150` is a constant, not a measurement** — too
   small on jittery links (late frames still drop), wasted latency on clean
   ones.

Pacing (T2) attacks 1+2; the adaptive offset (T3) attacks 3; interpolation
(T4–T5) attacks the residual 30-on-60 sample-and-hold judder that no pacing
can remove.

docs/15 Decision 4 rejected sink-side holding because decoder output frames
come from a bounded frame pool — holding ~9 of them (150 ms @ 60 fps) risks
stalling the decoder. That objection is answered here not by ignoring it but
by **layering**: the pre-decode pacer stays and keeps the pool exposure
bounded (≤3 held frames, just-in-time release), while the sink does only the
final sub-frame alignment (Decision 4 below).

## Decisions

1. **Measure the paint, not a proxy.** Three new metrics, all flowing
   unconditionally through the existing 500 ms `ViewerStats` path:
   - **Presentation-cadence error** (the headline metric): recorded at the
     inner draw in the render-sink layer (worker path; the main-thread path
     reports null — same precedent as `renderedFps`). For each consecutive
     pair of distinct drawn frames,
     `err = (draw-wall-clock delta) − (frame-timestamp delta in ms)`; if
     presentation perfectly tracked capture cadence, `err ≡ 0`. Published
     per stats window via Welford running stats (zero memory):
     `renderCadenceStdDevMs` + `renderCadenceP95Ms`. A raw draw-interval
     stddev is *not* enough — it conflates source cadence changes (fps rung
     switches) with jitter; the timestamp-delta subtraction is what makes it
     a jitter metric.
   - **Arrival jitter** (`arrivalJitterMs`): windowed p95 − windowed min of
     the arrival delta the reorder buffer already observes
     (`receivedAtMs − timestampMs` in `insert()`), via the new quantile
     tracker (Decision 2).
   - **Decode jitter** (`decodeJitterMs`): stddev of per-frame decode time
     per stats window in `handleDecoded()` — nearly free (the timings behind
     `lastDecodeLatencyMs` already exist), and it is the number that sizes
     `DECODE_LEAD_MS` (Decision 4) honestly.
   - **Overlay-gating is rejected explicitly**: stats already flow every
     500 ms from the worker; the per-frame cost is a couple of adds and a
     histogram increment at ≤60 Hz. Gating on overlay visibility would add a
     worker command for an unmeasurable win.

2. **One `WindowedQuantileTracker`, shared by measurement and control.**
   Same 60 s / 1 s-bucket shape as `WindowedMinTracker`
   (`transport/live-edge.ts`), but each bucket holds a small fixed-width
   histogram (e.g. 4 ms bins clamped to a bounded range above the bucket
   min) — O(window/bucket × bins) memory, O(1) observe, pure module with an
   injected clock, vitest-covered like its sibling. Resets on the
   broadcaster-restart signal exactly as the min tracker does. T1's metric
   and T3's offset controller read the **same** estimator — the measurement
   and the control loop cannot disagree.

3. **`PacedPresentationSink` subsumes `CoalescingRenderSink`.** Wrapping or
   stacking two rAF-scheduled sinks would add a frame of latency and fight
   over frame choice; instead one sink owns the single rAF loop and the
   pending-frame lifecycle.
   - The sink contract extends to `draw(frame, targetDisplayMs?)` — target
     absent ⇒ present ASAP (exactly today's coalescing behavior). With no
     targets the sink **is** `CoalescingRenderSink`: hold ≤1, newest wins,
     ≤1 inner draw per tick; the existing coalescing tests port over as its
     no-target mode. `CoalescingRenderSink` is then deleted.
   - With targets: hold at most **3** decoded frames. VRAM is trivial
     (~12 MB at 4K NV12); the real bound is the decoder frame pool, kept
     safe by the decode lead (Decision 4) — steady state holds 1–2 frames,
     3 only under transient bursts. Overflow closes the oldest
     (latest-frame-wins under load is structural, not a mode).
   - Per rAF tick: present the newest held frame whose
     `targetDisplayMs ≤ tickNow + halfDisplayInterval`; close every older
     held frame (counted as `pacingDropped`); if none due, draw nothing.
     A frame whose slot is more than one source interval past is closed
     unseen. Display interval = windowed median of rAF callback deltas (a
     small pure helper, unit-testable).
   - `flush()` (close all held, present nothing) is called on broadcaster
     restart, on `requestResync()` firing, on the mode toggling off (then
     present newest immediately), and in `stop()`. Backpressure semantics
     are untouched: decoder-queue-deep still triggers `requestResync()`;
     sink holds are invisible to `decoderQueueDepth`.
   - Where worker rAF is unavailable the schedule falls back to
     `setTimeout(16)` as today — pacing accuracy degrades to ~today's and
     the overlay says so (`presentation: paced (raf) / paced (timer) /
     immediate`).

4. **Layered pacing, not double pacing.** In `'adaptive'` mode the reorder
   buffer's release gate retargets from `target` to
   `target − DECODE_LEAD_MS` (**~35 ms**, one 30 fps interval — to be
   re-sized from T1's measured decode jitter in T6): coarse pre-decode
   pacing keeps the decoder frame-pool exposure bounded (the docs/15
   Decision 4 objection), the sink does the final ±½-vsync alignment.
   `REORDER_TICK_MS = 16` stays; its jitter is absorbed by the sink hold.

5. **A separate new toggle; three playout modes** (user decision
   2026-07-15). **Partly superseded by Decision 10 (2026-07-23)**: the
   menu now carries one smoothing toggle, "Paced playback", and the
   fixed-mode entry is dev-only. The three-mode enum, the module state and
   the worker command are unchanged — only the menu narrowed.
   The existing right-click "Smooth playback" toggle keeps its
   exact current behavior — fixed 150 ms decoder-release pacing,
   coalescing-equivalent presentation. A **new** right-click toggle,
   **"Paced playback (adaptive)"**, enables the new machinery: sub-frame
   presentation pacing + adaptive offset + decode-lead retarget. The two
   are mutually exclusive in the menu (checking one unchecks the other).
   Internally `transport/playout.ts` grows a `PlayoutController` (name amended at implementation from the sketch's `PlayoutScheduler`) plus module mode state, with
   mode `'off' | 'fixed' | 'adaptive'` — still a module-scoped instance per
   JS context, read live per advance, so the existing mid-session toggle
   semantics carry over; the worker `playout` command widens from a boolean
   to the mode enum, persisted in localStorage like the existing toggle.
   Default remains `'off'` (live edge).

6. **Adaptive offset with asymmetric slew** (`'adaptive'` mode only).
   `targetOffsetMs = clamp(arrivalP95 − arrivalMin + HEADROOM_MS,
   MIN_PLAYOUT_OFFSET_MS, MAX_PLAYOUT_OFFSET_MS)` with proposed constants
   `HEADROOM_MS = 34` (one 30 fps interval), `MIN = 50`, `MAX = 350`
   (inside `MAX_BUFFERED_FRAMES` ≈ 1.07 s @ 60 fps, well under
   `KEYFRAME_WAIT_MS = 1000`). `PLAYOUT_OFFSET_MS = 150` survives both as
   the fixed mode's constant and as the adaptive **seed** until the jitter
   window has ~5 s of data. Recompute 1×/s; **increases apply fast**
   (~50 ms/s slew — under-buffering means visible drops *now*), **decreases
   slow and reluctantly** (only after the target has sat ≥30 ms below
   current for ~15 s, then ~5 ms/s) — the `media/fallback.ts`
   step-down-fast/probe-up-slow philosophy. Slew rather than step: a slewed
   offset is a fractional-percent playback-rate nudge (invisible); a step is
   a visible skip or pause. No slider — adaptivity is exactly what makes a
   slider unnecessary. The overlay `Playout` row reads `live-edge` /
   `fixed (+150 ms)` / `adaptive (+NNN ms)` with `playoutOffsetMs` reporting
   the live value — a rising row on a degrading link is the visible cost,
   per the project's the-trade-is-never-silent philosophy. Trackers reset
   with the restart signal; the offset re-seeds at 150.

7. **Interpolation is an experiment with a pre-registered kill criterion.**
   Survey verdicts:
   - **(a) Linear blend/crossfade — rejected in advance as the shipped
     look** (ghosting on game motion is universally judged worse than 30 fps
     sample-and-hold) **but built anyway as the scaffold** (T4): it
     exercises 100 % of the plumbing — two-texture pipeline, doubled output
     cadence, α-slot scheduling, toggle, stats — with a 3-line shader, and
     calibrates the A/B methodology before any hard shader work.
   - **(b) Block-matching optical-flow-lite in WebGL2 fragment shaders —
     the recommended experiment and the learning payload** (T5): luma
     downscale pyramid → block match (16×16 blocks, ±16 px search at ¼
     res) into a motion-vector FBO → smoothing pass → bidirectional warp
     with occlusion fallback to the nearer source frame. Multi-pass
     ping-pong FBOs, expected a-few-ms GPU cost per synthesized frame at
     1080p; expected artifacts (edge halos, shimmering HUD elements) are
     named up front. WebGL2-only.
   - **(c) WebGPU ML (tiny RIFE-like) — rejected for scope**: a runtime
     dependency (onnxruntime-web/WebNN), tens of ms of inference at 720p+,
     and model shipping — a different project.
   - **Placement**: an `InterpolatingWebGLRenderSink` inside the WebGL
     layer (keeps the previous frame's texture, exposes
     `drawInterpolated(alpha)`), **driven by the paced sink's slots** —
     pacing already owns display timing, so 30→60 means each source
     interval gets two vsync slots: the real frame at α=0, one synthesized
     at α=0.5. This is why interpolation **requires `'adaptive'` mode on**
     (the fixed mode has no presentation slots to drive it).
   - **Own toggle, tightly gated**: "Frame interpolation (experimental)" in
     the right-click menu, default off, persisted, offered only when the
     sink is WebGL2 on the worker path *and* paced mode is on. Firefox/2D/
     main-thread never see the menu item; there is no 2D implementation,
     ever.
   - **Latency cost** (amended at implementation): the scaffold synthesizes
     **opportunistically** — a mid frame is produced only when the next real
     frame is *already in hand* (the decode lead + adaptive offset make that
     the common case at 30 fps), so it adds **no latency of its own**;
     an interval whose next frame hasn't arrived simply isn't interpolated.
     The design sketch's "+1 source interval" applied to guaranteed
     synthesis, which was not implemented. `capToRenderMs` is unchanged by
     the toggle.
   - **Kill criteria (pre-registered, evaluated in T5)** — interpolation is
     removed (toggle deleted, findings kept) if any of: (i) T1's
     render-cadence σ with interpolation on is **worse** than with pacing
     alone (GPU cost defeating the purpose); (ii) side-by-side on real game
     footage, viewers judge the artifacts worse than the 30 fps judder
     (subjective, but recorded — this is a hobby project for friends; their
     eyes are the metric); (iii) total measured glass-to-glass exceeds
     500 ms on the reference LAN setup with default settings.

8. **Defaults — SUPERSEDED 2026-07-15 (user decision after T1–T4 landed).**
   The design originally kept live-edge (mode `'off'`) as the default.
   Later the same day the user flipped it: the **production viewer now
   defaults to `'adaptive'` paced playback with frame interpolation on**;
   the right-click menu is the disable path (each toggle unchecks back to
   live-edge / interpolation off, persisted). Scope and safeguards of the
   flip:
   - It is a **UI-preference default** (`ViewerScreen`'s stored-preference
     loaders). The pipeline-level module default stays `'off'` — the frozen
     `#/debug/*` surfaces and any context that never receives the playout
     command keep live-edge behavior.
   - **Explicit choices survive migration**: a stored new-key mode always
     wins; the legacy boolean's `'1'` migrates to `'fixed'` and its `'0'`
     (an explicit live-edge choice) to `'off'` — the flip never overrules a
     viewer who opted out.
   - Interpolation-on is a no-op wherever the pipeline can't interpolate
     (main-thread path, non-WebGL2 sink); nothing breaks on Firefox.
   - Everything else stands: the fixed mode is preserved as-is; every
     drop/resync policy — gap grace, keyframe wait, queue-deep resync,
     frame cap — fires unchanged in every mode. Smoothing adds bounded
     delay, never patience: pacing holds only frames already in hand, and
     never waits longer for missing ones.

9. **Worker-first, honest degradation.** Main-thread pipeline path: no
   pacing, no interpolation, null cadence metrics (existing `renderedFps`
   precedent). Timer-fallback pacing is labeled in the overlay. Firefox
   regresses in nothing it has today and gains pacing wherever worker rAF
   exists (≥105).

10. **`'fixed'` retired from the production menu — pacing is one binary
    (2026-07-23, user decision).** Decision 8 kept `'fixed'` as a
    co-equal, mutually-exclusive toggle beside `'adaptive'`. That was
    wrong on the merits: **adaptive dominates fixed at every point on the
    trade curve**, so the pair was never a real choice.

    | | `'fixed'` | `'adaptive'` |
    |---|---|---|
    | Offset, clean link | 150 ms always | down to `MIN_PLAYOUT_OFFSET_MS` = 50 |
    | Offset, lossy link | 150 ms always | up to `MAX_PLAYOUT_OFFSET_MS` = 350 |
    | First ~`OFFSET_WARMUP_MS` | 150 ms | 150 ms (seed) — identical |
    | Presentation alignment | **none** | sub-frame slot matching |
    | Interpolation reachable | no | yes |

    The decisive row is the fourth. `viewer.ts`'s `displayTargetMs()`
    returns `undefined` outside `'adaptive'`, so `PacedPresentationSink`
    degenerates to R10's latest-frame-wins coalescing: **fixed mode paid
    the full buffering latency and then painted unpaced.** It bought a
    fraction of what its cost implied. And because adaptive's clamp floor
    (50 ms) sits *below* fixed's constant, fixed is not even the
    lower-latency option — there is no link condition, and no transient
    (the warmup seeds at the same 150 ms), where a viewer is better off
    with it.

    Secondary, but real: the viewer menu had grown to two smoothing
    toggles + three delivery modes + interpolation, and under any non-live
    delivery mode *both* smoothing entries render as inert controls
    annotated "governed by Resilient mode" (Decision 7 of docs/24). Going
    from two mutually-exclusive toggles to one binary removes a state pair
    a viewer cannot reason about.

    **What is kept, and why.** `'fixed'` survives as a `PlayoutMode` — in
    the type, in `getPlayoutOffsetMs()`, in the overlay's Playout row —
    because a **measurement-free offset is the control that separates a
    pacing bug from a bug in the thing measuring the pacing**, and
    PLAYOUT-1 (docs/24 finding 8) is proof that is a live failure mode:
    the arrival-jitter histogram's range silently pinned the resilient
    clamp at ~534 ms, and every symptom pointed at pacing. A constant
    offset is how you tell those apart in one toggle.

    So the mode is **gated on `isDevEnvironment()`** — the existing
    pattern from the broadcaster's dev settings (`showDevSettings`) —
    rather than left as an unreachable branch with a story attached. A
    diagnostic nothing can reach is dead code; this one is reachable
    exactly where a diagnostic is used. Labelled with its constant
    ("Smooth playback (fixed 150 ms)"), because the constant is the point.

    **Migration** (both hops land a viewer where the menu has a control):
    - stored `'fixed'` → `'adaptive'` outside a dev build; honoured inside
      one.
    - legacy `gawk:smoothed-playout === '1'` → `'adaptive'` (was
      `'fixed'`). That viewer asked for smoothing in the R5 UI; adaptive
      is the mode that now delivers it. `'0'` → `'off'` is unchanged —
      Decision 8's rule that the default flip never overrules an explicit
      live-edge opt-out still stands.

    **Naming.** The survivor is labelled **"Paced playback"**, dropping
    the "(adaptive)" qualifier — it existed only to distinguish it from
    the fixed mode. "Smooth playback" was deliberately *not* recycled for
    it: reusing a shipped label for different semantics would make every
    pre-2026-07-23 bug report about "Smooth playback" ambiguous.

    Nothing else moves: no server, wire, broadcaster or pipeline change;
    `PacedPresentationSink`, `PlayoutController` and every drop/resync
    policy are untouched, and the pipeline-level module default stays
    `'off'` so `#/debug/*` keeps live-edge.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| T1 | **Jitter measurement foundation** — `WindowedQuantileTracker` (`transport/live-edge.ts`); presentation-cadence recorder at the inner draw; decode-jitter window in `handleDecoded()`; `ViewerStats` fields `renderCadenceStdDevMs`, `renderCadenceP95Ms`, `arrivalJitterMs`, `decodeJitterMs` + Delivery overlay rows + Copy-diagnostics | Unit tests (fake clock/schedule, written first): quantile tracker returns correct p95 on known distributions, ages buckets out, resets on the restart signal; cadence error ≡ 0 for perfectly paced synthetic draws and nonzero for jittered ones; fields forward through the worker unchanged; overlay renders the rows; zero server/wire diffs; **baseline judder numbers recorded** (Chrome + Firefox, 30 fps source on a 60+ Hz display) | ✅ implemented 2026-07-15; manual verify done 2026-07-19 (baseline judder numbers → T6, not started) |
| T2 | **Paced presentation sink + mode plumbing** — `PacedPresentationSink` replacing `CoalescingRenderSink`; `draw(frame, targetDisplayMs?)`; ≤3 held frames; display-interval estimator; `flush()` wiring (restart/resync/toggle-off/stop); reorder release retarget to `target − DECODE_LEAD_MS`; `presentation` stat + overlay row; new "Paced playback (adaptive)" toggle with `'off'/'fixed'/'adaptive'` mode enum (worker `playout` command widened, mutual exclusion in the menu, persisted) | No-target mode reproduces all existing coalescing tests; `'off'` and `'fixed'` paths behaviorally identical to today (existing smoothing untouched — its tests still pass); slot semantics unit-tested with fake schedule + clock (due frame presented in its slot, early frame held, stale frame closed + counted); queue never exceeds 3 (oldest closed); `flush()` closes all; toggling the mode off mid-session presents newest immediately; reorder tests: release at `target − lead` in `'adaptive'` only; with `'adaptive'` on, measured render-cadence σ (T1) drops vs baseline on a jittered synthetic source | ✅ implemented 2026-07-15; manual verify done 2026-07-19 (σ-drop measurement → T6, not started) |
| T3 | **Adaptive playout offset** — `PlayoutController` in `transport/playout.ts` (fed the shared jitter estimate from the reorder buffer's trackers via the pipeline's stats tick); clamp [50, 350], seed 150, `HEADROOM_MS = 34`, asymmetric slew/dwell; reorder buffer + pipeline consume the shared scheduler; overlay `adaptive (+NNN ms)`; `playoutOffsetMs` reports the live value | Fake-clock unit tests (written first): converges to p95 − min + headroom on synthetic jitter; clamps at both bounds; increases fast / decreases only after the dwell period; every change is slew-bounded (no step > rate × dt); restart re-seeds at 150; mode off → 0 immediately; `'fixed'` mode still returns the constant 150; worker `playout` command round-trips the mode | ✅ implemented 2026-07-15 |
| T4 | **Interpolation scaffold (experimental)** — `InterpolatingWebGLRenderSink` (ping-pong prev/curr textures, `upload()` decoupled from `present(alpha)`, linear-blend shader); α-slot scheduling in the paced sink (opportunistic mid-slot at α=0.5, 30→60); "Frame interpolation (experimental)" toggle (worker command, persisted, gated WebGL2 + worker + `'adaptive'` mode); `interpolation` stat + overlay row | Pure slot/α scheduling unit-tested (fake clock): mid-slot emitted only when both frames are in hand, real frames always presented at α=1 from the uploaded texture, frames closed on upload; no synthesis across gaps > 100 ms or across flush/resync; toggle absent on 2D sink / main-thread / fixed-or-off mode; `capToRenderMs` unchanged (opportunistic — see Decision 7 as amended); crossfade ghosting **observed and recorded** in manual verify (the expected outcome — it validates the A/B harness) | ✅ implemented 2026-07-15 (droppable); manual verify done 2026-07-19 (ghosting A/B observation → T6, not started) |
| T5 | **Motion-estimated interpolation** — block-match luma pyramid + MV smoothing + bidirectional warp/occlusion shaders replacing the blend; GPU-time budget check | Shader pipeline behind the same toggle; kill criteria (i)–(iii) from Decision 7 evaluated on the reference hardware (render-cadence σ, side-by-side judgment on real game footage, g2g < 500 ms on the reference LAN); GPU cost measured at 1080p and native; **an explicit keep/kill verdict recorded in this doc** | not started (droppable) |
| T6 | **Findings + sync pass** — measurement findings section in this doc (before/after tables from T1 metrics across T2–T5, Chrome + Firefox, LAN + one remote peer); keep/change verdicts for `DECODE_LEAD_MS`, the clamp bounds, `HEADROOM_MS`, the seed; README gotchas + ROADMAP status sync | Findings section with numbers from both browsers incl. one remote peer; every named constant gets a recorded verdict (changes land test-first as usual); README gotcha list updated if any landed; ROADMAP R12 row synced | not started |

Ordering: T1 → T2 → T3 → (T4 → T5) → T6. T4+T5 are independently droppable
as a unit; nothing after them depends on them. T3 could technically land
before T2, but measuring adaptivity without sub-frame pacing understates its
benefit.

## Implementation notes (2026-07-15)

T1–T4 implemented test-first the same day the doc landed; all automated
gates green (390 vitest tests, `tsc -b` via build, oxlint). Deviations from
the sketch, each folded back into the decisions above:

- **`PlayoutController`, not `PlayoutScheduler`** (Decision 5, amended): the
  controller class lives in `transport/playout.ts` beside the module mode
  state; the arrival min/quantile trackers **stayed in the reorder buffer**
  (they need its injected clock and restart signal) and the pipeline feeds
  the controller the same `arrivalJitterMs()` estimate the overlay shows,
  once per stats tick — measurement and control still read one estimator.
- **Descent arming vs. continuing** (Decision 6): the 30 ms margin only
  *arms* a descent; once armed and past the dwell, the offset slews all the
  way to the target (a red test caught the offset flooring at
  target + margin otherwise).
- **Interpolation is opportunistic** (Decision 7, amended): no +1-interval
  latency — a mid frame is synthesized only when the next real frame is
  already in hand; `capToRenderMs` is unchanged by the toggle.
- **The interpolating sink serves all WebGL2 rendering**: plain `draw()` is
  `upload()` + `present(1)`, so the two-texture path costs nothing when the
  toggle is off, and the toggle's visibility comes from
  `ViewerStats.interpolation` (`'on' | 'off' | null`) — ground truth from
  the pipeline, not UI guesswork.
- **Cadence discontinuity guard instead of restart plumbing** (Decision 1):
  the recorder drops any sample whose timestamp delta is non-positive or
  > 5 s (a broadcaster restart moves timelines), so no restart signal needs
  to reach the render sink for the metric's sake.
- **Persistence**: one mode key (`gawk:playout-mode`) replaces the legacy
  boolean (`gawk:smoothed-playout`, migrated to `'fixed'` on load — **now
  to `'adaptive'`, Decision 10**); interpolation persists under
  `gawk:interpolation`.
- **Default flip (2026-07-15, after T1–T4 landed)**: the production viewer
  now defaults to adaptive paced playback + interpolation (Decision 8, as
  superseded). Legacy `'0'` (explicit old-toggle opt-out) maps to `'off'`
  so the flip never overrules a prior explicit choice; interpolation
  defaults on via `gawk:interpolation !== '0'`.

## Verification plan (manual, per chunk — summarized)

- **T1**: open the overlay on a healthy LAN viewer — cadence σ visibly
  nonzero at 30-on-60 (the baseline); numbers recorded in this doc.
- **T2/T3**: on a jittery link (or devtools-throttled), toggle "Paced
  playback" — judder visibly decreases, cadence σ drops vs the
  T1 baseline, the Playout row shows the live adaptive offset rising on a
  degrading link and slewing back down (slowly) on recovery; toggling back
  to live-edge (and, in a dev build, to "Smooth playback (fixed 150 ms)")
  behaves exactly as today, no stall or reconnect.
- **T4/T5**: interpolation toggled on a 30 fps rung — rendered fps ≈ 2×
  received fps; `capToRenderMs` rises ≈ 33 ms; artifacts inspected on real
  game footage against kill criterion (ii); GPU cost sanity-checked in the
  performance panel.
- **No-regression**: Firefox worker path unchanged-or-better; main-thread
  fallback path still plays with new toggles absent; default (`'off'`) path
  behaves identically to today on both browsers.

## Non-goals

- ~~**Changing the live-edge default** — re-rejected; opt-in, per viewer,
  visibly costed.~~ **Superseded 2026-07-15** (user decision): adaptive
  paced playback + interpolation are now the production viewer's defaults —
  see Decision 8. Still per-viewer disableable and visibly costed (the
  overlay's Playout/latency rows).
- **FEC parity chunks / keyframe-burst taming** — surveyed 2026-07-15,
  deliberately not selected for R12; candidates for their own roadmap items.
- **Keyframe-request back-channel** — rejected for good (docs/15
  Decision 6); interpolation and pacing change nothing about that calculus.
- **WebGPU/ML interpolation** — rejected for scope (Decision 7c).
- **A playout-offset slider** — the adaptive controller replaces manual
  tuning; revisit only if T6's findings show the controller mis-sizing.
- **Server or wire changes of any kind** — everything here is
  viewer-client-side.
- **Audio/AV sync** — still no audio; unchanged from docs/15.
