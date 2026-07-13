# R4 — Automatic Resolution Fallback: Design + Implementation Plan

Design doc for [ROADMAP R4](../ROADMAP.md#r4--automatic-resolution-fallback).
The resolution picker gains an **auto** option — the new default. In auto
mode, if the broadcaster's machine can't sustain encoding, the pipeline
steps down the R3 resolution ladder automatically, and steps back **up**
when the encoder has been healthy for a while — with a visible indication
either way. Choosing **any explicit rung** (native/1080p/720p/480p) is
honored unconditionally: no automatic stepping in either direction, ever —
**frame drops are preferred over going against the broadcaster's explicit
choice**. R4 is deliberately a **pure delta on R3** (docs/08): the ladder,
the pre-encode scaling stage, and the mid-stream encoder-recreate machinery
all exist and are the actuation path; R4 adds only *detection* and the
*decision loop*.

## Goals

- Detect sustained encoder backpressure broadcaster-side, from signals the
  pipeline already produces — no new probes, no guesswork from metadata.
- **Auto mode (default)**: step down one rung at a time under sustained
  backpressure; step back up one rung at a time after a sustained healthy
  period. Oscillation is bounded by a post-step cooldown plus an
  exponential backoff on step-up probes.
- **Explicit mode**: an explicitly selected rung is never auto-stepped.
  Detection still runs, but only feeds a passive "encoder can't keep up"
  indication — it never actuates.
- One-off spikes (scene change, background task, window drag) must **not**
  trigger a downgrade — hysteresis is a hard requirement.
- Visible indication on the broadcast page: auto mode shows the currently
  applied rung ("auto — sending at 720p"); explicit mode shows the passive
  pressure warning when applicable.
- A mid-stream encoder *error* in auto mode steps down and recreates
  instead of killing the broadcast (bounded — repeated immediate failures
  still end it). Explicit mode keeps today's behavior (error is fatal).
- **Zero relay-server, wire-format, and viewer changes** — same property R3
  proved; the relay stays a byte forwarder.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| I1 | Fallback decision core, pure logic (`media/fallback.ts` `FallbackController`; `autoLadder` in `media/ladder.ts`) | Unit tests (written first) pass: sustained-rejection step-down at the ratio/window thresholds, spike immunity (short bursts never trigger), cooldown discards outcomes after any step, step-up probe fires after the sustained-healthy period, up-probe backoff doubles after a failed probe (step-down soon after step-up) and resets after a surviving one, `autoLadder` yields only rungs that actually shrink the source (4K → [native, 1080, 720, 480]; 1080p source → [native, 720, 480]; sub-480p source → [native]), error-path bounding (step-down vs. fail per Decision 7) | not started |
| I2 | Pipeline integration (`transport/broadcaster.ts`: `ResolutionSelection` plumbing, auto-mode rung state, outcome feed, decision actuation via the existing reset path, encoder-error rewire, pressure flag for explicit mode, stats fields) | Existing vitest suite green; new broadcaster-level tests cover: an explicit selection never steps under sustained rejections (only `droppedFrames` grows, pressure flag sets), auto mode steps down and later back up, switching selection mid-broadcast applies immediately (explicit) or restarts at the ceiling (auto), encoder error in auto steps down while a second error inside the bound fails, encoder error in explicit mode fails as today; `tsc -b` + lint green | not started |
| I3 | UI (`LadderPicker`, `broadcastSettingsStore`, `BroadcastPage.tsx` + `stream.module.css`) | Picker's resolution axis gains "auto" as the first/default option; store defaults to `'auto'` when the localStorage key is missing or invalid, previously persisted explicit rungs keep their meaning; auto-mode indicator shows the currently applied rung and updates on steps; explicit-mode pressure warning is display-only (no button, no actuation); stats grid gains auto/pressure rows; lint + build green | not started |
| I4 | Verification, tuning + docs sync | `npm test`, `npm run lint`, `npm run build` green; manual browser verification (below) passed, thresholds adjusted from real-hardware behavior if needed (constants are named in one place); README gotchas + ROADMAP + CLAUDE.md synced | not started |

Goal → verified-by, for the cross-cutting behaviors:

| Goal | Where | Verified by |
|------|-------|-------------|
| Sustained backpressure → one-rung step down (auto mode) | `fallback.FallbackController` | `fallback.test.ts` |
| Sustained health → one-rung step up (auto mode), backoff on failed probes | `fallback.FallbackController` | `fallback.test.ts` |
| Spikes never trigger; cooldown after every step | `fallback.FallbackController` | `fallback.test.ts` |
| Only rungs that actually shrink the source are used | `ladder.autoLadder` | `ladder.test.ts` |
| An explicit rung is never auto-stepped, in either direction | `broadcaster.ts` selection plumbing | `broadcaster.test.ts` |
| Encoder error: auto steps down (bounded), explicit fails as today | `broadcaster.ts` error rewire + controller | `fallback.test.ts` + `broadcaster.test.ts` |
| Broadcaster sees the auto rung / the pressure warning | BroadcastPage | manual verify |
| Viewers recover from an auto step like a manual one (≤ 1 keyframe interval) | R3 machinery, unchanged | manual verify |
| No visible quality pumping under steady overload (backoff engages) | controller backoff | manual soak |

## Decisions

1. **Detection signal: encode-queue rejection ratio.** `Encoder.encode()`
   already refuses frames when `encodeQueueSize > 2` — that refusal (today
   just counted as `droppedFrames`) *is* the backpressure symptom, measured
   at exactly the right place. The pipeline feeds every accept/reject
   outcome (plus a timestamp) to the controller; the controller triggers on
   a sustained rejection *ratio*. Why this signal and not the others the
   ROADMAP floated:
   - It is **self-normalizing to input rate**: a game rendering at 30 fps,
     or an fps-gated rung, produces fewer frames but no rejections — no
     false trigger. "Output fps below target" as a signal would need to
     distinguish "encoder slow" from "source slow"; rejection ratio doesn't.
   - Queue-depth sampling and encode latency are derivatives of the same
     condition, sampled worse (the 500 ms stats tick can alias a sawtooth
     queue).
   - Encoder *errors* are a separate, discrete signal — handled by
     Decision 7, not by the ratio.
2. **Hysteresis: sliding window + trigger ratio + post-step cooldown.**
   Initial constants (named in one place in `fallback.ts`, tuned in I4 on
   the real gaming PC):
   - `WINDOW_MS = 4000` — outcomes older than this fall out of the window.
   - `MIN_SAMPLES = 15` — never decide on fewer outcomes (guards low-fps
     rungs and startup; at the 5 fps rung a full window holds ~20).
   - `TRIGGER_RATIO = 0.25` — step down when ≥ 25 % of windowed frames were
     rejected. A struggling encoder converges on rejecting
     `1 − encodeRate/inputRate` of frames, so 0.25 ≈ "running 25 % behind".
   - `COOLDOWN_MS = 8000` — after *any* encoder recreate (auto step in
     either direction, selection change, source-dimension change) outcomes
     are discarded for the cooldown: renegotiation churn must not count,
     and the new rung gets a fair chance before the next evaluation.
   A scene-change spike saturates the queue for a few hundred ms — far
   short of 25 % of a 4 s window. A background task that steals the CPU for
   5+ seconds *should* trigger; that's the feature.
3. **Auto mode walks a per-source effective ladder that skips no-op
   rungs.** `autoLadder(srcLongerDim)` (pure, in `ladder.ts` next to the
   rung definitions) returns `'native'` followed by every resolution rung
   whose longer-dimension cap is *strictly below* the source's longer
   dimension — the rungs that actually shrink the picture. A 1920×1080
   source yields `[native, 720, 480]` (the 1080 rung's 1920 cap wouldn't
   change anything — stepping there would recreate the encoder for an
   identical config). Auto state is an index into this list: index 0 is the
   **ceiling** (start of every broadcast — optimistic), the last entry is
   the **floor**. A step-down demand at the floor takes no action and
   latches a distinct warning ("encoder can't keep up even at the lowest
   rung"), cleared by recovery or a selection change. If the *source*
   dimensions change mid-broadcast (window share resized), the ladder is
   recomputed and auto state resets to the ceiling — rare, and a fresh
   baseline beats guessing an equivalent index.
4. **"auto" is a selection, not a rung — and it is the new default.** The
   picker's resolution axis becomes `ResolutionSelection = 'auto' |
   ResolutionRung`; `ResolutionRung` and everything downstream
   (`computeTargetSize`, the preprocessor) stay concrete — the pipeline
   resolves `'auto'` to the current auto-ladder rung. An **explicit
   selection is honored unconditionally**: no stepping in either direction,
   no exceptions — sustained frame drops are preferred over overriding the
   broadcaster's stated choice. Detection still runs in explicit mode, but
   its only output is a passive pressure indication (Decision 8).
   `broadcastSettingsStore` persists the selection exactly as today with
   `'auto'` as the fallback for a missing/invalid key; a previously
   persisted explicit rung (including `'native'`) keeps its meaning — that
   user made an explicit choice under R3 semantics and now gets exactly
   what the label says.
5. **Step-up is automatic in auto mode, guarded by a probe interval with
   exponential backoff.** There is no signal for "the rung above *would*
   cope" short of trying it, so stepping up is try-and-observe, made safe
   by three layers:
   - **Probe trigger**: rejection ratio below `RECOVERY_RATIO = 0.02`
     continuously for `UP_PROBE_MS` (initial 30 000) → step up one ladder
     index. The normal 8 s cooldown follows, as after any step.
   - **Backoff**: if a step-down fires within `UP_FAIL_WINDOW_MS = 60000`
     of a step-up, that probe *failed* — the next probe waits double
     (30 s → 60 s → 120 s, capped at `UP_PROBE_MAX_MS = 480000`). A probe
     that survives the fail window resets the interval to the base value.
   - Worst-case steady overload therefore pumps quality at most once per
     probe interval, and the interval grows toward ~8 minutes — visible
     pumping decays instead of oscillating.
   Every step-up lands on the ceiling eventually when the machine truly
   recovered (throttling ended, background job finished), which is the
   point of auto mode: the broadcaster never has to notice.
6. **Auto state is runtime-only.** The current auto rung, probe backoff,
   and floor latch are pipeline state — never persisted, reset to the
   ceiling on every broadcast start and on any resolution-selection change.
   A **framerate** rung change keeps the current auto resolution rung (the
   encoder recreate it causes still clears the detection window via the
   cooldown): fps intent and resolution health are independent, and
   resetting to the ceiling on a 60→30 change would trigger a pointless
   re-descent.
7. **A mid-stream encoder error steps down instead of killing the
   broadcast — in auto mode only, once per bound.** Today `Encoder`'s error
   callback goes straight to `fail()` and the broadcast dies; a flaky
   hardware encoder at native resolution becomes a dead stream. In auto
   mode R4 rewires it: on a runtime encoder error, dispose the encoder and
   re-enter first-frame negotiation one rung down (the error is treated as
   the strongest possible backpressure evidence). Bounded: if an error
   arrives while already at the floor, or within `ERROR_FAIL_WINDOW_MS =
   10000` of a previous error-triggered reset, the pipeline fails for
   real — repeated immediate errors mean the problem isn't resolution. In
   **explicit mode the error stays fatal** (current behavior): silently
   switching resolution against an explicit choice is exactly what
   Decision 4 rules out. `configure()`-time probe failures are *not*
   touched in either mode: the existing codec/variant cascade already
   handles those.
8. **Explicit mode gets a passive pressure warning, nothing more.** The
   controller runs regardless of mode; in explicit mode a would-be
   step-down decision merely sets a `pressure` flag (cleared by the same
   sustained-healthy condition as Decision 5's probe trigger) that the UI
   renders as "encoder can't keep up at 1080p — frames are being dropped;
   consider auto or a lower rung". Display-only: no button, no actuation,
   no state beyond the flag. The detection is already computed; showing it
   costs nothing and spares the broadcaster diagnosing `droppedFrames`.
9. **Bitrate reduction is not a fallback rung.** The ROADMAP asked; the
   answer is no: bitrate already follows the ladder (`computeBitrate` runs
   on every recreate with the new dimensions), and backpressure here is
   compute-bound — encoding the same pixel count at a lower bitrate costs
   roughly the same CPU/ASIC time, so a bitrate-only rung wouldn't relieve
   the queue. Bandwidth-motivated reduction is the broadcaster's manual
   ladder, and per-viewer adaptation stays out of scope (ROADMAP non-goal).
   Framerate is likewise not auto-stepped: the resolution ladder alone
   spans ~16× in pixels, and 5 fps is a deliberate mode, not a degradation
   target.
10. **The controller is pure and timer-free.** `FallbackController`
    receives `(accepted, nowMs)` per encode attempt and returns a decision;
    time is injected, never read. Evaluation happens on each fed outcome —
    there is nothing to poll, and no decision can fire while no frames flow
    (an idle stall is not encoder backpressure; the step-up probe likewise
    requires *observed healthy frames*, not mere elapsed time). Same
    testability discipline as `FpsGate` and `KeyframeCadence`.

## Why the relay and viewer need no changes

An auto step — in either direction — is *mechanically identical* to the
manual mid-stream rung change R3 shipped and verified: the preprocessor
target changes, the encoder is recreated, the first chunk out carries fresh
`decoderConfig` metadata, `handleEncoded` prepends the rebuilt config
datagram to the keyframe, the relay re-caches config + keyframe, connected
viewers reconfigure and recover within one keyframe interval, late joiners
get the cached pair. See "Why the viewer and relay need no changes" in
[docs/08](08-resolution-framerate-picker.md) — every step of that walkthrough
applies verbatim; R4 only changes *who pulls the trigger*. `nextFrameId`
keeps counting across resets, so viewer-side ordering is undisturbed.

## Proposed Changes

### `gawk-app`

#### [NEW] src/media/fallback.ts

`FallbackController` — pure decision core per Decisions 1, 2, 5, 7, 10:

- `record(accepted: boolean, nowMs: number): 'none' | 'stepDown' | 'stepUp'`
  — appends to the sliding window, evaluates both the trigger ratio and the
  step-up probe, and enters cooldown internally when it emits a step.
- `onEncoderError(nowMs: number): 'stepDown' | 'fail'` — Decision 7
  bounding (the auto/explicit split lives in the pipeline, which ignores
  `'stepDown'` outside auto mode and fails instead).
- `noteReset(nowMs: number)` — called by the pipeline on *any* encoder
  recreate it initiated itself (selection change, source-dimension change)
  so the cooldown and window clear regardless of who caused the reset.
- `stepRejected(direction)` — the pipeline reports a step it could not
  apply (already at floor/ceiling), so the controller can latch the floor
  state / suppress repeat probes instead of re-firing every frame.
- All threshold constants live here, exported for the tests.

No DOM, WebCodecs, or clock access. Ladder knowledge stays out — the
controller decides *direction*, the pipeline resolves it against
`autoLadder`.

#### [NEW] src/media/fallback.test.ts

Written before the implementation (CODE-REVIEW.md discipline): steady 30 %
rejection over a full window steps down exactly once then cools down; a
100 % rejection burst shorter than `MIN_SAMPLES`/window never triggers;
outcomes during cooldown are discarded; window slides (old outcomes
expire); healthy outcomes for `UP_PROBE_MS` emit `stepUp`; a step-down
within `UP_FAIL_WINDOW_MS` of a step-up doubles the next probe interval,
capped; a surviving step-up resets the interval; error inside
`ERROR_FAIL_WINDOW_MS` of an error-reset → `'fail'`; error after
`stepRejected('down')` at the floor → `'fail'`; healthy stream never steps.

#### [MODIFY] src/media/ladder.ts (+ ladder.test.ts)

- `ResolutionSelection = 'auto' | ResolutionRung` +
  `RESOLUTION_SELECTIONS` (auto first — it is the default).
- `autoLadder(srcLongerDim: number): ResolutionRung[]` per Decision 3.
  Test cases: 3840 → `[native, 1080, 720, 480]`, 1920 → `[native, 720,
  480]`, 1280 → `[native, 480]`, 854 and below → `[native]`.

#### [MODIFY] src/transport/broadcaster.ts

- `setLadder()` takes `ResolutionSelection`; a resolution-selection change
  resets auto state to the ceiling (Decision 6), an explicit selection
  drives the preprocessor directly as today.
- Auto-mode state: the `autoLadder` for the current source dimensions + the
  current index; the preprocessor target is set from the resolved rung.
- Frame callback feeds `controller.record(accepted, performance.now())`
  after each `encode()` call on a live encoder. In auto mode a
  `'stepDown'`/`'stepUp'` decision moves the ladder index (or reports
  `stepRejected` at floor/ceiling), updates the preprocessor, and flags the
  encoder reset — the exact `setLadder` actuation path. In explicit mode
  `'stepDown'` only sets the pressure flag (Decision 8).
- Encoder `onError` rewired through `controller.onEncoderError` in auto
  mode per Decision 7; explicit mode keeps the direct `fail()`.
- Self-initiated resets (selection change, source-dimension change) call
  `controller.noteReset()`.
- `BroadcastStats` gains `autoRung: ResolutionRung | null` (null when not
  in auto mode), `autoAtFloor: boolean`, `autoStepDowns: number`,
  `autoStepUps: number` (soak-test observability), and
  `encoderPressure: boolean` (explicit-mode warning).

#### [MODIFY] src/features/stream/LadderPicker.tsx, src/state/broadcastSettingsStore.ts

- Resolution `<select>` gains "auto" as the first option; the store's
  resolution axis becomes `ResolutionSelection` with `'auto'` as default
  and validation fallback (existing persisted explicit rungs load
  unchanged, per Decision 4).

#### [MODIFY] src/features/stream/BroadcastPage.tsx (+ stream.module.css)

- Auto-mode indicator (distinct from the error style) whenever the applied
  rung is below the ceiling: "Auto: sending at 720p — encoder couldn't keep
  up higher"; at-floor variant: "Encoder can't keep up even at 480p — try a
  lower framerate or close other apps." No buttons — recovery is automatic.
- Explicit-mode passive warning when `encoderPressure` is set: "Encoder
  can't keep up at 1080p — frames are being dropped. Consider auto or a
  lower rung." Display-only.
- Stats grid gains "Auto rung" (`—` / rung / `at floor`) and
  "Auto steps (down/up)" rows.

### `gawk-server`

No changes (see "Why the relay and viewer need no changes"). No wire-format
changes either — the config-precedes-keyframe contract does all the work.

## Verification Plan

### Automated Tests

- `fallback.test.ts` — controller cases listed above.
- `ladder.test.ts` — `autoLadder` cases listed above.
- `broadcaster.test.ts` — wiring: explicit selection never steps under
  sustained rejections (pressure flag only); auto mode steps down and later
  back up; selection change mid-broadcast applies immediately (explicit) or
  restarts at the ceiling (auto); framerate change keeps the auto rung;
  encoder error in auto → recreate one rung down, second error inside the
  bound → pipeline `onError`; encoder error in explicit mode → `onError` as
  today.
- Existing suites stay green — wire format, viewer, relay untouched.

### Manual Verification

On the real setup (Chromium broadcaster, ≥ 1 viewer), inducing backpressure
with DevTools CPU throttling (Performance panel, 4×–20× slowdown) on the
broadcaster tab — most reliable when the encoder negotiated a *software*
variant, so pick a high-resolution source; note in the doc afterwards what
throttle factor was needed against the hardware path:

1. **Auto trigger + viewer recovery**: auto (default) on a ≥ 1440p source,
   apply throttling → within ~`WINDOW_MS + COOLDOWN_MS` the auto indicator
   appears, the stats "Sending" row drops a rung, and the viewer recovers
   at the lower resolution within ≤ 1 keyframe interval, no reconnect.
2. **Progressive descent + floor**: keep throttling high → further steps
   respect the cooldown spacing; at the last rung the at-floor warning
   appears and stepping stops.
3. **Automatic recovery**: remove throttling → after ~`UP_PROBE_MS` per
   rung the stream climbs back to the ceiling and the indicator clears.
4. **Oscillation backoff**: leave throttling at a level that fails exactly
   the ceiling rung → probes step up, fail, step back down, and the
   interval between pumps visibly grows (check `autoStepUps` pacing over a
   multi-minute soak).
5. **Spike immunity**: brief load bursts (a second or two) at a sustainable
   rung → `autoStepDowns` stays 0 across a multi-minute soak.
6. **Explicit rung is sacred**: select 1080p, apply heavy throttling →
   resolution never changes, `Dropped (source)` grows, the passive warning
   appears; remove throttling → warning clears. Repeat with `native`.
7. **Selection changes mid-broadcast**: auto (stepped down) → explicit 720p
   applies immediately and stops all stepping; explicit → auto restarts at
   the ceiling and re-descends only if pressure persists.
8. **Late joiner during fallback**: join a new viewer after an auto step —
   first picture is at the stepped-down resolution (relay cache, R3
   behavior).
