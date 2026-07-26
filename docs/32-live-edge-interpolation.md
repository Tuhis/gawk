# R27 — Frame Interpolation in Live-Edge Mode

Design doc for [ROADMAP R27](../ROADMAP.md#r27--frame-interpolation-in-live-edge-mode)
(designed 2026-07-25; **revised in owner review** — the mechanism moved from
arrival-triggered to **timestamp-scheduled** blends (owner-directed) and
Decision 4's default-on carry-over was **accepted**; a second pass added the
**variable-framerate policy** — game fps swings with GPU load, so `H` slews
with engage/disengage dwell (Decision 2) and the "Variable framerate is the
normal case" section frames the regimes; a third pass (2026-07-26, owner
decision) **simplified A/V sync to a fixed ≈16.7 ms audio delay** —
sub-deadband, so `H` never touches the audio schedule and the
schedule-motion machinery evaporates (Decision 5, which records the
superseded schedule-coupled variant); **not started**). Extends R12 T4's experimental frame interpolation
([docs/17](17-viewer-playback-smoothing.md) Decision 7) to the **live-edge**
playout mode (`playoutMode 'off'`), where it is currently structurally
unreachable. The accepted cost — named up front because it is the whole
trade — is a presentation **hold offset `H` of about one source frame gap**
(≈17 ms at 60 fps, ≈33 ms at 30 fps), applied only while interpolation has
something to buy: static content, stalls, resyncs and 60 fps-on-60 Hz
sessions pay **zero** added latency by construction.

Feasibility verdict first: **feasible, and smaller than the first draft.**
Presenting frames at timestamp-derived slots is exactly what the T4 paced
machinery already does — held frames with display targets, due-slot
selection, early-upload, `midSlotMs` mid-blends — so the sink needs
essentially nothing new. The change is: the pipeline supplies
`displayTargetMs = timestamp + arrival baseline + H` in live-edge when
interpolation is active (today `displayTargetMs()` returns `undefined`
outside adaptive mode — that gate is the whole reason live-edge can't
interpolate), plus a small pure **hold-offset policy** computing `H` from
source-timestamp gaps, plus stats/menu gating. The arrival baseline is
already maintained in every mode (the reorder buffer's `WindowedMinTracker`
observes unconditionally). Zero server, wire, and broadcaster changes; zero
new worker messages (the `interpolation` command already crosses into the
viewer worker and sets module state regardless of mode).

## Goal

A viewer who has deliberately turned **Paced playback off** — the
lowest-latency mode — can still turn 30 fps content into 60 fps presentation
with the existing "Frame interpolation (experimental)" toggle, at a bounded,
visible cost of about one source frame interval, instead of interpolation
silently ceasing to exist the moment pacing is off. The four combinations of
the two toggles become a coherent ladder on the latency/smoothness curve:

| Paced | Interp | What you get | Added latency vs. pure live-edge |
|-------|--------|--------------|----------------------------------|
| ✓ | ✓ | today's default | adaptive offset (50–350 ms, jitter-tracked) |
| ✓ | ✗ | paced, sample-and-hold | adaptive offset |
| ✗ | ✓ | **this item** | `H` ≈ one source frame gap (≤ `MAX_LIVE_EDGE_HOLD_MS`) |
| ✗ | ✗ | pure live-edge | 0 |

## Background: why live-edge never interpolates today

Interpolation synthesizes a mid-frame as `blend(frame N, frame N+1, 0.5)` — it
needs **both endpoints in hand**. In adaptive mode that comes free: the paced
sink holds ≤3 decoded frames against display targets, so when frame N's
mid-slot comes due, frame N+1 is usually already held
(`render-sink.ts` `tick()`, the `uploadedNext` machinery), and the synthesis
is opportunistic with zero added latency — docs/17 Decision 7's design.

In live-edge mode, `viewer.ts#displayTargetMs()` returns `undefined` (mode ≠
`'adaptive'`), so every frame takes the sink's *immediate* path: latest frame
wins, painted at the next rAF tick, never held. At the moment frame N paints,
frame N+1 does not exist yet anywhere in the pipeline. There is no scheduling
trick that gets around this — **synthesizing a blend requires the left
endpoint to still be presentable when the right endpoint arrives**, i.e. a
hold. The user-facing framing is correct: at least one frame of latency is
the price of admission, and this design's job is to make that price exact,
bounded, and paid only when the purchase (a synthesized mid-frame) actually
happens.

### Why not just use adaptive mode at its floor?

The honest comparison, since adaptive + interpolation already exists:

- **Adaptive at its floor** adds ≥ `MIN_PLAYOUT_OFFSET_MS` (50 ms) on a clean
  link and *grows with measured jitter* (clamp ceiling 350 ms) — its offset
  is a jitter envelope, sized to make (nearly) every frame hit its slot.
- **Live-edge + hold offset** adds `H` ≈ one source gap — **16.7 ms at
  60 fps, 33 ms at 30 fps** — and can never grow beyond
  `MAX_LIVE_EDGE_HOLD_MS` regardless of jitter, because `H` is derived from
  the **frame interval, not from jitter**. The consequence is the honest
  trade: frames whose arrival jitter exceeds `H` miss their slot and present
  ASAP (a cadence blip — never worse than live-edge today), and intervals
  whose next frame arrives too late simply aren't interpolated. Adaptive
  buys near-guaranteed smoothness with latency; this mode buys bounded
  latency with opportunistic smoothness.

On the reference hardware path the project measures ~50 ms glass-to-glass
(docs/15 Q4). Adaptive's floor *doubles* that; `H` at 30 fps raises it to
~83 ms. For the viewer who opted out of pacing precisely because they want
minimum latency, that difference is the reason this item exists.

## The mechanism: a timestamp-scheduled hold offset

*(Revised in owner review 2026-07-25 — supersedes the first draft's
arrival-triggered hold-one; see Decision 1 for the recorded rejection.)*

In live-edge with interpolation active, the pipeline gives each decoded frame
a display target on the same schedule shape adaptive mode uses:

```
target(N) = ts_N / 1000 + arrivalBaseline + H
```

`arrivalBaseline` is the reorder buffer's existing windowed-min of
`(arrival − timestamp)` — the jitter-cancelling anchor `displayTargetMs()`
already uses in adaptive mode, maintained in every mode today. `H` is the
hold offset (Decision 2). Everything downstream is the **unmodified T4
machinery**: the paced sink holds the frame to its slot (±½ vsync), uploads
the next real frame early, and synthesizes `present(0.5)` at
`midSlotMs(prev, next)` when both endpoints are in hand.

The feasibility arithmetic that sizes `H` (with `gap = ts_{N+1} − ts_N` and
`j` = a frame's arrival jitter above the baseline):

- Frame N makes its own slot iff `j_N ≤ H`.
- The mid-blend for [N, N+1] is due at `target(N) + gap/2`; N+1 is decoded
  and in hand by then iff `j_{N+1} ≲ H − gap/2` (decode latency eats into
  the margin, which is why release stays immediate — Decision 3).

With `H = one median gap`, mids synthesize whenever next-frame jitter stays
under about half a frame interval — the common case on a clean link — and
presentation cadence is **timestamp-true**: blends land at the midpoint of
the source timeline mapped through one affine schedule, not at jittery
arrival instants. Frames that miss their slots degrade per-frame to today's
live-edge behavior (newest due wins, present ASAP), never worse.

Properties worth pinning:

- **No parallel hold mechanism.** The first draft's deadline presents,
  once-only observer guards and supersede logic all dissolve: targets are
  absolute times, so the last frame of a motion burst presents at its own
  slot whether or not a successor ever arrives — the screen can never be
  left stale waiting for an arrival.
- **No decoded-frame budget change**: the sink holds ≈ `⌈H / gap⌉` frames
  (≤ 2 at `H` = one gap), inside `MAX_HELD_FRAMES = 3`; Decision 2 bounds
  the worst case and docs/20 field finding 2's lesson (release schedule and
  display schedule must not diverge past the held-frame budget) is honored
  explicitly.
- **Added latency = `H`**, constant-ish, structurally tied to the frame
  interval, hard-capped at `MAX_LIVE_EDGE_HOLD_MS`.
- **Cadence metrics come alive**: because presents run through the paced
  path, `renderCadence*` and the T1 jitter instrumentation now measure
  live-edge interpolation for free.

### Variable framerate is the normal case

Game fps swings with GPU load — 60 in menus, 25–45 in heavy scenes,
occasional 100 ms+ hitches — and the broadcaster's gate is drop-only (never
CFR conversion, an R14/R3 invariant), so the timestamps the viewer sees
reflect that truthfully. The policy is designed for this shape, not for a
constant-fps ideal:

- **The mode is a dips smoother.** Near display Hz there is nothing to buy
  (no mid rAF tick fits) and `H` disengages to 0 — pure live-edge. In the
  heavy-load dips (~20–40 fps) — exactly where 60 Hz sample-and-hold judder
  is most visible — it engages and doubles the presentation rate. Across
  hitches and static stretches the per-interval legality bound
  (`MAX_INTERPOLATION_GAP_MS`) refuses the blend and the median refuses to
  move. The feature concentrating its effect (and its one-frame cost) in
  the regime where it helps most is the correct product outcome, not a
  degradation.
- **`H` is necessarily a prediction, and both error directions degrade
  gracefully.** A frame's target is committed when it is decoded — one
  interval before the next gap is knowable — so no policy can be exactly
  right while fps varies; the design's job is cheap errors. Too small (fps
  just dropped): the next frame isn't in hand by the mid slot and the
  interval simply isn't interpolated — plain live-edge until the slew
  catches up. Too large (fps just rose): a few ms of extra hold inside the
  cap and one more held frame inside the budget. Never a stall, never
  latency past `MAX_LIVE_EDGE_HOLD_MS`.
- **Blend *placement* is exact even when the prediction is wrong.** The mid
  slot is computed from the two frames' **actual** targets —
  `midSlotMs(target(N), target(N+1))`, i.e. their real timestamps through
  the affine schedule — not from `H`'s guess about the gap. Variable fps
  therefore cannot mistime a blend; the prediction only decides whether a
  given interval blends at all. (A property the rejected arrival-triggered
  design did not have, and a second reason the timestamp-scheduled revision
  wins under game load.)
- **The remaining hazard is video presentation-timing motion**: a game
  oscillating around an engage boundary must not flap the video cadence,
  and tracking a sustained fps change must not step it — Decision 2's
  slew + dual-threshold dwell machinery exists for exactly this. Audio is
  deliberately insulated from all of it: Decision 5's fixed sub-deadband
  delay means `H`'s motion never reaches the audio schedule.

## Decisions

1. **Timestamp-scheduled presentation through the existing paced machinery**
   (owner decision 2026-07-25). Rejected alternatives, recorded so they
   aren't re-derived:
   - *Arrival-triggered hold-one* (the first draft: present N when N+1
     arrives, mid at half-gap, deadline present at full gap). Its virtue was
     needing no schedule at all; its two real defects decided the review:
     blends inherit **arrival jitter** (the very double-image shimmer the
     kill criterion feared), and the hold is invisible to the video
     presentation schedule, so **A/V sync could only be patched after
     measurement** rather than being correct by construction. It also needed
     a parallel hold path in the sink (deadline logic, once-only guards)
     that the timestamp design gets from existing, tested machinery.
   - *Adaptive with a micro-profile* (clamp floor ≈ one frame interval):
     still rejected — its offset is a **jitter envelope** (p95 − min +
     headroom) that grows to its ceiling under loss, which is the opposite
     of the structural one-gap bound; and adaptive-at-50 ms already exists
     for anyone who wants jitter-sized pacing. The distinction that
     survives this revision: *how the offset is derived* (frame interval vs.
     jitter), not *how frames are timed* (both are timestamp-scheduled now).
   - *Holding in the reorder buffer* (release N when N+1 is buffered): holds
     frames **pre-decode**, so the hold and N+1's decode serialize instead
     of overlapping, exactly the margin the mid-slot inequality needs.
   - *Pinning `H` to the broadcaster's framerate rung*: the rung is not on
     the wire, 'auto'/'native' selections have no fixed number, and — the
     premise of the variable-framerate policy — actual delivered fps sits
     *below* the rung whenever the game loads the GPU. The docs/01
     principle applies to time as much as pixels: trust the timestamps of
     the frames in hand, never configuration metadata.
   - *Sizing `H` to a high percentile of the gap distribution* (p75/p90, so
     nearly every interval can blend under varying fps): sizes every
     frame's latency to the slowest regime — a 60 fps game that dips to 30
     would pay ~33 ms even while running at 60. Guaranteed smoothness
     bought with latency already exists; it is called adaptive mode.
   - *Extrapolation* (synthesize forward from N, no hold): a linear blend
     cannot extrapolate; anything that can is T5's motion-estimation
     territory plus hallucinated content on top. Rejected.
2. **The hold offset `H` is derived from source timestamps — slewed, never
   stepped, with engage/disengage dwell.** A small pure policy (in
   `interpolation.ts`, beside `midSlotMs`): the *target* is the **median**
   inter-frame timestamp gap over a short window (`H_WINDOW_MS ≈ 4 s`;
   median, not mean, so a single 150 ms GPU hitch moves it not at all),
   clamped to `[0, MAX_LIVE_EDGE_HOLD_MS = 67]`, and `H` **slews** toward
   it (`H_SLEW_MS_PER_S ≈ 15`, both directions) rather than stepping — the
   R12 lesson verbatim: the paced sink turns offset changes directly into
   presentation cadence, so a slewed `H` is an invisible fractional-rate
   nudge where a step would be a skip. (Since Decision 5's simplification
   `H` never touches the audio schedule, so the slew serves **video
   cadence alone** — it needs no sizing against the audio trim.) The
   slew's risk asymmetry is the *inverse* of adaptive's: too-small `H` is
   cosmetic (an interval isn't interpolated), too-large `H` is latency —
   so neither direction is urgent and both stay gentle. Zeroing rules make
   the mode free when there is nothing to buy, each with **dual thresholds
   + a dwell** (`H_ENGAGE_DWELL_MS ≈ 5 s`, the fallback.ts
   hysteresis/cooldown pattern), because a 0 ↔ `H` transition is the one
   *step* slewing cannot remove — a game oscillating around a boundary
   must not flap the video presentation timing (audio is insulated by
   Decision 5's constant):
   - **Worthwhileness**: engage when the median gap sustains above ~1.7 ×
     the display interval, disengage below ~1.3 × (no mid rAF tick can fit
     otherwise — 60 fps on a 60 Hz display never engages; ~35 fps engages,
     and it takes a recovery past ~46 fps to disengage). The sink's
     existing `DisplayIntervalEstimator` is exposed read-only for this.
   - **Low-fps refusal**: disengage when the median gap sustains above
     `MAX_LIVE_EDGE_HOLD_MS` — ≲15 fps content would pay a big hold for
     ghosty blends. (`MAX_INTERPOLATION_GAP_MS = 100` stays the
     per-interval blend-legality bound in the sink, unchanged — with a
     near-constant `H` the latency no longer depends on the individual
     gap, so the first draft's per-gap 67 ms cap is not needed there.)
   - **Held-frame budget**: `H` is additionally capped at ~2 × a **low
     percentile** (p10) of recent gaps — deliberately not the strict
     minimum, which damage-driven capture's occasional back-to-back frames
     would crush — so a 60 fps burst under a stale larger `H` cannot ask
     the sink to hold > `MAX_HELD_FRAMES` frames (docs/20 field finding 2's
     failure shape); overflow beyond that still degrades to
     latest-frame-wins, a blip not a stall.
   The window, slew rate, dwell and thresholds are named provisional
   tunables (the `KEYFRAME_WAIT_MS` convention); LI4 sets them against real
   variable-fps game footage. `H > 0` requires: mode `'off'` ∧
   interpolation enabled ∧ the sink `supportsInterpolation` ∧ arrival
   baseline established. Everywhere else the pipeline passes no targets and
   the immediate path runs byte-identical to today.
3. **Release stays immediate — the reorder buffer is untouched.** Frames
   decode on arrival (live-edge's existing behavior) and the sink is the
   only holder. Deliberately *not* adaptive's release-at-target-minus-lead:
   decoding ahead maximizes the mid-slot margin (N+1 must be **decoded** by
   the mid, and `H` is small), keeps the decoder warm, and keeps this
   feature out of the release-gate/`releasableAt` machinery entirely. The
   schedule gap this opens between release and presentation is `H` ≤ 2
   frames — inside the held-frame budget by Decision 2's cap.
4. **One toggle governs both modes** — **accepted (owner decision
   2026-07-25)**. The existing "Frame interpolation (experimental)" entry
   and `gawk:interpolation` key; no new setting. The `ViewerScreen` menu
   gate widens from
   `(resilientMode || playoutMode === 'adaptive') && stats.interpolation != null`
   to just `stats.interpolation != null`, and `viewer.ts` reports
   `stats.interpolation` capability-based (`supportsInterpolation`, any
   mode) — which also simplifies the docs/24 finding-16 gate. Accepted
   consequence: interpolation defaults on (`gawk:interpolation !== '0'`),
   so a viewer who turns Paced playback off lands on live-edge + `H`
   (≈1 frame) rather than pure live-edge unless they also untick
   interpolation — the ladder in the Goal table is coherent, both controls
   sit in the same menu, and the overlay shows the cost (Decision 6).
5. **A/V sync: a fixed audio delay, not schedule coupling** (owner decision
   2026-07-26, simplifying and superseding this doc's earlier
   schedule-coupled design — recorded below so it isn't re-derived). Audio
   is delayed by a constant **`LIVE_EDGE_INTERP_AUDIO_DELAY_MS ≈ 16.7`**
   (one 60 fps interval) whenever the live-edge interpolation gate is open
   — applied on the audio-alignment side (av-sync, where audio consumes
   the video schedule); `videoScheduleBaseEpochMs()` is untouched and the
   live `H` **never touches any audio-visible schedule**. Why the constant
   is good enough, made explicit:
   - **16.7 ms is below the audio rate trim's 20 ms deadband** (docs/20
     field finding 4) — the property that makes the whole simplification
     safe: every transition (toggling interpolation, `H` engaging or
     disengaging) moves the audio-relevant schedule by at most a
     sub-deadband constant, absorbed silently with zero machinery. The
     superseded design coupled audio to the live `H` and therefore needed
     slew-vs-trim sizing, dwell-limited engage-step absorption and a
     finding-11 re-anchor boundary; all of that evaporates. (A constant of
     ~33 ms would center the engaged regime better but sits *above* the
     deadband, buying back exactly the transition machinery this deletes —
     rejected.)
   - When the mode is **engaged**, content is ≤ ~35 fps by construction
     (Decision 2's worthwhileness rule), so `H` ≥ ~28 ms and the constant
     undershoots: the residual is an audio *lead* of `H − 16.7` — ~11–16 ms
     at 30 fps, under the ~45 ms lead-detection threshold and under the
     deadband. On high-refresh displays (120/144 Hz), where 60 fps content
     does engage with `H` ≈ 16.7 ms, the constant is **exact**.
   - When the toggle is on but `H` is disengaged (60 fps on 60 Hz), audio
     *lags* 16.7 ms — the tolerant direction, far inside targets.
   - Worst engaged case (`H` near the 67 ms cap, ≲15 fps content): ~50 ms
     audio lead — borderline detectable, but that is the ghosty-blend
     disengage boundary of an experimental mode; accepted, and visible in
     the overlay.
   Deliberate consequence, recorded: the audio-visible schedule now
   *approximates* live-edge video presentation within a small bounded
   error instead of tracking `H` — for audio, schedule **stability** is
   worth more than schedule accuracy at these magnitudes (a rate trim
   handles a constant bias far better than a moving target). docs/20's
   video-master rule is respected (audio adapts; video is never
   rescheduled). `avSkewMs` — sampled at the actual paint (field finding
   9) — verifies the residuals on real hardware, and the constant is a
   cheap knob if the field numbers disagree (LI4).
6. **The cost is visible (live-edge philosophy).** New overlay row
   **"Interpolation hold"** in the Delivery section: the live `H` (0 when
   inactive). The `presentation` stat becomes honest about *pacing being in
   effect* rather than inferring it from the mode: it reports
   `paced-raf`/`paced-timer` whenever the pipeline is supplying display
   targets (adaptive **or** live-edge hold) and `immediate` otherwise,
   while `playoutMode` continues to read `'off'` — the overlay thereby
   distinguishes all four ladder rungs. Recorded measurement caveat, same
   class as docs/17 T4's: `capToRenderMs` and `liveEdgeDriftMs` are sampled
   at decoder output and therefore **exclude** the sink-side hold (in
   adaptive mode the buffer-side offset dominates and *is* included). The
   hold row is the honest number; the existing metrics' definitions are
   deliberately not changed.
7. **Kill criteria (pre-registered, evaluated in LI4)** — live-edge
   interpolation is removed (or confined behind `isDevEnvironment()`) if,
   on the reference hardware at a 30 fps rung on a typical clean link,
   either: (i) the **interpolated-interval rate is too low to matter** —
   fewer than ~half of eligible intervals actually synthesize a mid (the
   jitter-vs-`H/2` margin failing in practice would mean the mode pays its
   latency for sporadic 60 fps moments, which reads worse than honest
   30 fps); or (ii) side-by-side against plain live-edge, the result reads
   *worse* (slot-missing cadence blips + intermittent blends can look
   choppier than sample-and-hold); or (iii) measured added latency exceeds
   ~1.5 × the median source gap (the policy failing its own promise). A
   documented rejection is a valid completion — T4/T5 precedent, docs/17
   Decision 7.

## What does not change

- **Zero server, wire, broadcaster changes.** Viewer client only.
- **The reorder buffer is untouched** — release stays immediate in
  live-edge (Decision 3); `releasableAt`, decode lead, baselines, restart
  machinery all as today (the pipeline's existing restart path additionally
  resets the `H` estimator, as it already resets the playout controller).
- **The paced sink's code path is the existing one**: held-frame slots,
  `uploadedNext`, `midSlotMs`, `MAX_INTERPOLATION_GAP_MS`,
  `MAX_HELD_FRAMES` — reused, not modified. Adaptive mode's behavior is
  byte-identical (its targets are computed exactly as today).
- **Resilient / Deep buffer modes are unaffected** — they force effective
  adaptive (docs/24 Decision 7), so the live-edge gate never opens there.
- **Main-thread pipeline, 2D and WebGL1 sinks, `#/debug/*`**: no
  interpolating sink ⇒ no targets, `stats.interpolation` stays null, menu
  entry stays absent — the same capability gate as today, minus the mode
  condition.
- **Interpolation toggled off, or `H = 0`**: the pipeline passes no targets
  and the immediate path runs exactly as today — toggle-off must be
  indistinguishable from pre-R27.
- **T5 (motion-estimated interpolation)** still slots behind the same
  `upload()`/`present(α)` surface and would upgrade both modes at once.

## Chunks

| Chunk | Work | Acceptance criteria | Status |
|-------|------|---------------------|--------|
| LI1 | **Hold-offset policy + pipeline targets (pure, test-first)**: `H` estimator in `interpolation.ts` (windowed median of source-timestamp gaps, slew, dual-threshold engage/disengage + dwell, p10 held-frame cap — Decision 2; `MAX_LIVE_EDGE_HOLD_MS = 67`); expose the sink's display-interval estimate read-only; `viewer.ts#displayTargetMs()` supplies `ts + base + H` under the live-edge gate; restart resets the estimator | Unit tests (fake clock): targets supplied only under the full gate (mode `'off'` ∧ enabled ∧ `supportsInterpolation` ∧ baseline ∧ `H > 0`); `H` ≈ median gap at 30 fps and 0 on 60 fps-on-60 Hz (worthwhileness), 0 past the low-fps bound; **variable-fps behavior pinned**: a sustained 60→30 fps swing slews `H` (per-update delta bounded by the slew rate — never a step), fps oscillating around an engage boundary flips 0 ↔ `H` at most once per dwell, a single 150 ms hitch leaves the median target unmoved, and during slew lag the affected intervals present unblended with no stall; a 60 fps burst under stale `H` never asks the sink to hold > `MAX_HELD_FRAMES` (p10-gap cap; overflow = latest-wins, no stall — sink test); frames with jitter > `H` present ASAP (existing due-path, test pins it); restart/backwards timestamps reset; interpolation-off and `H = 0` paths byte-identical to pre-R27; all existing paced-path and reorder-buffer tests green unmodified | not started |
| LI2 | **Gating + observability**: `stats.interpolation` capability-based (any mode); `ViewerScreen` menu gate reduced to `stats.interpolation != null`; "Interpolation hold" overlay row (Delivery section) showing live `H`; `presentation` stat keyed on targets-in-effect rather than mode; docs/13 playbook note (rendered ≈ 2× received now also valid in live-edge; hold row + `playoutMode 'off'` is this mode's signature) | Menu entry present in live-edge on the worker+WebGL2 path and absent on main-thread/2D (existing tests extended); overlay row shows 0 when inactive and ≈ one gap while active; `presentation` reads `paced-*` with `playoutMode 'off'` when `H > 0`, `immediate` when interpolation is off; docs/24 finding-16 regression test still green with the simplified gate | not started |
| LI3 | **Fixed A/V audio delay**: apply `LIVE_EDGE_INTERP_AUDIO_DELAY_MS ≈ 16.7` on the audio-alignment side (av-sync) whenever the live-edge interpolation gate is open; `videoScheduleBaseEpochMs()` and the `H` policy stay audio-uncoupled; hardware `avSkewMs` pass | Test-first: audio start alignment shifts by exactly the constant when the gate is open and by 0 otherwise; `H`'s motion (slew, engage/disengage) provably never reaches the audio schedule (test pins the absence, the docs/20 finding-4 pattern); adaptive-mode and interpolation-off paths byte-identical; toggling interpolation with audio live transitions cleanly (sub-deadband — no trim churn, no re-anchor); hardware pass records `avSkewMs` median/p95 within docs/20 targets on 30 fps content (expect ≈ `H` − 16.7 ≈ 16 ms lead) and 60 fps content (expect ≈ 16.7 ms lag), short healthy sessions | not started |
| LI4 | **Verification + verdict**: manual browser pass (30 fps rung → rendered ≈ 2× received in live-edge, hold row ≈ 33 ms, timestamp-true blend cadence; 60 fps rung → `H = 0`, no latency delta; latency A/B vs. adaptive-at-floor and plain live-edge); **measure the interpolated-interval rate** (kill criterion (i)); evaluate criteria (ii)/(iii) on **real variable-fps game footage** (GPU-load swings — the dips regime is where the mode earns its keep, and where flapping would show); tune `H_WINDOW_MS`/`H_SLEW_MS_PER_S`/`H_ENGAGE_DWELL_MS`, the engage thresholds and `MAX_LIVE_EDGE_HOLD_MS` against it, and decide whether `H` needs a small measured jitter margin (bounded by the cap); record findings + an explicit keep/kill verdict in this doc | The doc's verification table filled in; kill criteria explicitly evaluated with a recorded verdict (keep, confine, or remove — any is a valid completion); constants confirmed or retuned with rationale, incl. the slew/dwell numbers under real GPU-load swings | not started |

Ordering: LI1 → (LI2 ∥ LI3) → LI4. LI3's hardware leg and LI4 need real
audio hardware and should follow R15's pending re-verification for the same
reason R25's sequencing note gives — `avSkewMs` is the instrument, and
finding 12 (over-reporting on long/stressed sessions) is still open; measure
on short, healthy sessions. No e2e-harness step in v1: the R20 tier-1 job
runs the adaptive default and already proves the interpolation machinery end
to end; a third seeded viewer pass would spend constrained CI minutes
(docs/25) on what LI1's unit tests and LI4's manual pass cover — revisit only
if a regression escapes.

## Verification plan (LI4 fills this in)

| Check | How | Result |
|-------|-----|--------|
| 30 fps → 60 presentation in live-edge | 30 fps rung, paced off, interp on; overlay rendered ≈ 2× received | — |
| Hold cost ≈ one gap | overlay Interpolation hold ≈ 33 ms at 30 fps; latency A/B confirms | — |
| 60 fps pays nothing | 60 fps rung on 60 Hz display: hold row 0, no latency delta vs. interp off | — |
| Interpolated-interval rate | kill criterion (i): ≥ ~half of eligible intervals synthesize on a clean link | — |
| Blend cadence | timestamp-true mids (side-by-side vs. plain live-edge — kill criterion (ii)) | — |
| Latency A/B | capture→render + hold vs. adaptive-at-floor and plain live-edge | — |
| A/V skew (LI3) | avSkewMs median/p95, audio on, short healthy sessions: ≈ `H` − 16.7 lead at 30 fps, ≈ 16.7 lag at 60 fps; toggle transition clean (sub-deadband) | — |
| Variable-fps game load | real game swinging 60↔30 under GPU load: `H` slews (no video-timing steps), engage state stable (≤1 flip per dwell), blends resume after slew catch-up, audio unaffected (fixed delay — no schedule motion) | — |
| Damage-driven capture | static screen → motion burst → static: `H` sticky, no stale frame, no held-frame overflow | — |

## Non-goals

- **Motion-estimated interpolation** — stays T5, unchanged, behind the same
  surface.
- **A jitter-envelope offset for live-edge** (growing `H` to make every
  frame hit its slot) — that is adaptive mode; Decision 1 keeps the line:
  `H` is derived from the frame interval, and jitter beyond it degrades
  gracefully instead of buying latency.
- **Extrapolation / latency-free synthesis** — rejected (Decision 1).
- **Auto-selecting the mode for the viewer** — the toggles stay manual.
