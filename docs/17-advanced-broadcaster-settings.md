# R12 — Advanced broadcaster settings

**Status**: designed 2026-07-15; not started. Supersedes R7
(hardware-supported controls & capture constraints), which is absorbed here.

## Goal

Rehaul the broadcaster's resolution/framerate settings into a
hardware-acceleration-aware settings system. Two places in the pipeline care
about resolution and framerate — the capture (`getDisplayMedia` +
`MediaStreamTrackProcessor`) and the encoder — and today they are only
loosely coupled: capture always runs at native/4K@60 while the encoder chews
whatever the preprocessor hands it. In the ideal session both run at the same
resolution and framerate with hardware encoding. After R12:

- The browser's encoder capabilities are **probed up front** (a support
  matrix over resolution × framerate × codec × acceleration), and the default
  configuration is the best one that hardware acceleration supports.
- An **advanced settings** panel lets the broadcaster choose the
  acceleration mode (auto / hardware only / software only), resolution,
  framerate, and — as diagnostics/learning controls — bitrate and codec
  overrides. Picker options are annotated from the probe matrix ("software
  only", "unsupported") instead of silently degrading.
- **Capture follows the sticky selection** via live
  `track.applyConstraints()` — no stream restart, no re-picking the screen —
  while the R4 auto-fallback keeps stepping at the encode level only.

## Current state (what gets rehauled)

- `media/capture.ts` `acquireDisplayStream()`: requests `width ≤ 3840,
  frameRate ideal 60` unconditionally; the only HW awareness is a crude gate
  (`probeHardwareSupport`: >1080p **and** >30 fps with no HW support → cap
  the *capture request* to 30 fps).
- `media/encoder.ts` `initEncoder()` path re-runs the same >1080p@>30 →
  cap-to-30 gate (`setCappedFps`), then walks the codec × variant cascade
  (`prefer-hw + realtime` → `prefer-hw` → `realtime` → `default`) with
  `isConfigSupported()`.
- Rungs are applied purely pre-encode (`FramePreprocessor`), so a 720p30
  session still pays full 4K@60 capture — measured on the gaming PC as the
  *source-limiting* cost during R4 verification (docs/09).
- The fps default is 30 as a deliberate **fan-out cap** (build-order item
  11), not an encoder limit.

## Locked decisions

1. **R12 supersedes R7.** Both R7 bullets — hardware-aware UI controls and
   capture-constraint propagation — live here; R7 is marked superseded in
   the ROADMAP. R7's third bullet (request the rung's resolution directly in
   `getDisplayMedia`) is **rejected** in favor of Decision 6's
   broad-grant-then-constrain shape.
2. **Probe matrix** (`media/probe.ts`): probe
   (resolution rung × framerate rung × codec preference × acceleration hint)
   via `VideoEncoder.isConfigSupported()` into a support map — each
   (rung, fps) pair resolves to `'hardware' | 'software' | 'unsupported'`
   plus the winning codec. Non-native rungs probe at their 16:9 canonical
   dims (1920×1080 / 1280×720 / 854×480); the native rung probes at
   3840×2160 as a pre-capture upper bound and is **refined from the first
   real frame's dimensions** once capture starts (frames are truth —
   docs/01). Probes run when the broadcaster surface loads and are cached
   in-memory per page load only (no persisted cache — probes are fast and a
   stale GPU answer is worse than a few ms of re-probing).
   *Assumption carried from the existing code, to verify in L1*: on
   Chromium, `prefer-hardware` + `supported: true` is a commitment to
   hardware. On Firefox every combo resolves software (its `VideoEncoder`
   is software-only) — the matrix degrades to annotations, never blocks.
3. **HW-aware auto ceiling.** The 'auto' resolution selection stays the
   default, but its ceiling is no longer unconditionally 'native': it is the
   **highest rung whose (rung, effective fps) probes hardware** under the
   current acceleration mode and codec override — where the effective fps is
   the explicit framerate rung, or the resolved 'auto' framerate of
   Decision 4 (with both axes on auto, the joint default resolves
   framerate-first). Below the ceiling, the R4
   machinery is untouched — step-down/step-up, cooldowns, backoff, explicit
   rungs never auto-stepped. Step-up probes stop at the ceiling. If nothing
   probes hardware (Firefox; software-only mode), the ceiling is **1080p**
   — a sane software starting point; R4 handles the rest. Pure helper in
   `ladder.ts` (slice `autoLadder()` at the ceiling), unit-tested.
4. **The framerate default is probe-driven too — an 'auto' framerate
   selection.** The framerate axis gains
   `FramerateSelection = 'auto' | FramerateRung` (mirroring R4's resolution
   shape), and 'auto' becomes the default. It resolves at probe time,
   **framerate-first**: if any resolution rung probes hardware at 60 fps,
   'auto' resolves to **60** and the resolution ceiling is computed at 60;
   otherwise it resolves to **30** (ceiling computed at 30). It never
   resolves to 'native' — a 144 Hz monitor would multiply the fan-out for
   no viewer-visible benefit. Unlike the resolution axis, 'auto' fps does
   **not** runtime-step: R4 stepping stays resolution-only, and the fps
   choice re-resolves only when the probe re-runs (acceleration mode /
   codec / source change). This **consciously revises build-order item 11's
   30 fps fan-out default**: with hardware encode confirmed by the probe,
   60 fps is the default experience and the doubled datagram rate + viewer
   decode load are accepted; explicit 30 / 5 rungs stay one click away, and
   the software path (nothing probes hardware — Firefox, software-only
   mode) keeps the conservative 30.
5. **Tri-state acceleration control**: `hwPreference: 'auto' | 'hardware' |
   'software'`, persisted in `broadcastSettingsStore`.
   - `auto` (default): today's cascade — prefer hardware, silently fall
     back to software (with the existing one-line console explanation).
   - `hardware`: only `prefer-hardware` variants are tried. In auto
     resolution mode the ceiling already guarantees a HW-capable rung; an
     *explicit* rung that can't do HW **fails to start with a clear error**
     instead of silently going software — that refusal is the point of the
     mode (verifying NVENC actually engaged).
   - `software`: only `prefer-software` variants (a `prefer-sw + realtime` /
     `prefer-sw` pair mirroring the HW ones) — the R4-tuning and
     comparison-benchmark mode.
   The encoder's `CONFIG_VARIANTS` walk becomes a filter over this
   preference; `classifyAcceleration` and the `EncoderConfigured.acceleration`
   surface carry over.
6. **Capture alignment = broad grant + live `applyConstraints`.**
   `getDisplayMedia` keeps requesting the broad native grant (unchanged —
   the grant is the whole surface); immediately after acquisition, and on
   every change of the **sticky target** (explicit rung, or the auto
   ceiling — *not* auto steps), the pipeline applies
   `{ width: { max: cap }, frameRate: { max: fps } }` on the capture track.
   The browser downscales/decimates at the track level, so MSTP delivers
   target-sized frames — the capture-side cost drops without ever
   re-prompting the screen picker. Raising the selection later re-applies
   constraints upward against the same broad grant, so **no settings change
   ever requires a stream restart** (see the restart table below).
   `applyConstraints` rejection or under-delivery is non-fatal by
   construction: the `FramePreprocessor` stays in place as the safety net,
   and encoder config keeps deriving from the actual frames in hand
   (docs/01 rule) — constraints are an optimization, frames are truth.
7. **Auto fallback stays encode-only.** R4 step-downs/step-ups below the
   ceiling never touch capture: they target software-encode CPU pressure,
   they must be cheap and instantly reversible, and an up-probe needs the
   higher-resolution source still flowing to have anything to step up to.
   This is also why the sticky target — not the current auto rung — drives
   Decision 6.
8. **Worker path: constraints apply to the transferred clone,
   worker-side.** Track clones hold independent constraints, and the R11
   worker owns the encode-source clone — so capture alignment crosses as a
   worker command (folded into the existing `setLadder` message shape) and
   the worker calls `applyConstraints` on its track. The main-thread
   original keeps the broad grant (the preview stays native — a feature).
   Whether a transferred clone honors `applyConstraints` downscaling in
   Chromium is the design's main open risk — **L3 starts with that spike**;
   if it doesn't, the worker path simply keeps preprocessor-only scaling
   (today's behavior) and main-thread capture alignment still lands.
9. **Probe-driven picker annotation.** Resolution/fps options are badged
   from the matrix: hardware (unmarked), "SW" (software only), disabled
   ("unsupported"), with tooltips naming the reason. Options are annotated,
   never removed — explicitly choosing a software rung is allowed (R4's
   "explicit choices are honored" philosophy). Annotations recompute when
   the acceleration mode or codec override changes.
10. **The ad-hoc 30 fps caps are removed.** The `probeHardwareSupport`
    gates in `acquireDisplayStream` and `initEncoder` (+`setCappedFps`) are
    subsumed: the auto ceiling covers the default path, and an explicit
    >1080p@60 software choice is annotated and honored instead of
    force-capped — today's cap silently overrides an explicit rung, which
    contradicts the R4 philosophy.
11. **Bitrate override** (advanced): numeric override replacing
    `computeBitrate()`'s ladder value, default "auto (ladder)". Absolute —
    it applies unchanged across rung changes and auto steps until reset to
    auto. Clamped to [0.5, 50] Mbps (the homelab's 1 Gbps uplink allows
    experiments well past the ladder's 10 Mbps cap; 15 viewers × 50 Mbps is
    still within egress limits, and the R2 bandwidth cap backstops abuse).
12. **Codec override** (advanced): pin one codec from
    `DEFAULT_CODEC_PREFERENCES` (friendly name + codec string), default
    "auto (negotiate)". A pin makes the preference list length one
    everywhere — encoder walk and probe matrix alike. A pinned codec that
    can't configure fails to start with a clear error (annotation warns
    first).
13. **Runtime truth over probe truth.** If the live `configure()` disagrees
    with the probe (lands software where the probe said hardware, or
    fails), the runtime result wins and is surfaced: the production
    broadcaster overlay gains an **Encode row** (actual codec +
    hardware/software from `EncoderConfigured`), so the probe is advisory
    and the session is observable. In `hardware` mode a software landing is
    treated as a configure failure (Decision 5).
14. **Zero server / wire / viewer changes.** Pipeline, settings store, and
    broadcaster UI only. The frozen `#/debug/broadcast` page keeps the old
    behavior (it constructs `BroadcastPipeline` with defaults and no
    advanced settings).

## Restart semantics (the "when do we restart?" answer)

| Change | Capture effect | Encode effect | Stream restart? |
|---|---|---|---|
| Explicit resolution/fps rung change | `applyConstraints` (live) | encoder recreate (existing `dimsChanged`/reset path) | never |
| Auto step-down / step-up (R4) | none | encoder recreate (existing) | never |
| Auto ceiling / auto-fps re-resolve (probe refinement, mode/codec change) | `applyConstraints` (live) | encoder recreate | never |
| HW tri-state change | none | encoder recreate (variant filter changes) | never |
| Bitrate / codec override change | none | encoder recreate | never |
| `applyConstraints` rejected / ignored | none (preprocessor covers) | none | never |

The broad-grant-then-constrain shape (Decision 6) is what makes the last
column uniform: nothing the settings can express exceeds the original
`getDisplayMedia` grant, so there is never a reason to re-prompt. The only
remaining restarts are the pre-existing ones (user stops sharing, session
death).

## Chunks

### L1 — probe matrix core

`media/probe.ts`: `probeSupportMatrix(rungs, fpsRungs, codecs, hwPreference)`
→ per-(rung, fps) `SupportEntry { acceleration: 'hardware' | 'software' |
'unsupported', codec: string | null }`, canonical probe dims per rung,
native-rung refinement from real source dims, in-memory memoization.
Injectable `isConfigSupported` for tests. Verify (manually, on the gaming
PC + Firefox) and document the prefer-hardware commitment assumption.

| Acceptance criterion | Verified by |
|---|---|
| Matrix classifies HW/SW/unsupported per (rung, fps) from a mocked `isConfigSupported` | `probe.test.ts` |
| First codec preference that resolves at a combo wins; codec pin narrows the walk to one | 〃 |
| Native rung refines when given real source dims (re-probe at actual size, memo invalidated) | 〃 |
| Probe exceptions classify as unsupported, never throw out of the matrix | 〃 |
| Firefox-shaped mock (all prefer-hardware rejected) yields an all-software matrix | 〃 |

### L2 — encoder tri-state + overrides

`media/encoder.ts`: variant filtering by `hwPreference` (new prefer-software
variant pair; `hardware` mode tries only prefer-hardware variants and
treats a software landing as failure); bitrate override and codec pin
plumbed through `initEncoder`'s config computation in
`transport/broadcaster.ts`; remove the `initEncoder` fps cap +
`setCappedFps` (Decision 10).

| Acceptance criterion | Verified by |
|---|---|
| `hardware` mode never configures a software variant; exhaustion throws the clear-refusal error | `encoder-configure.test.ts` |
| `software` mode probes only prefer-software variants | 〃 |
| `auto` mode reproduces today's cascade byte-for-byte (existing tests pass unmodified) | existing tests |
| Bitrate override replaces `computeBitrate` output, clamped [0.5, 50] Mbps; 'auto' restores ladder math | `broadcaster.test.ts` |
| Codec pin reduces the preference walk to one codec; unsupported pin fails start with a typed error | 〃 |
| The >1080p@>30 force-cap is gone: explicit 4K@60 + software mode configures at 60 | 〃 |

### L3 — HW-aware auto ceiling + capture alignment

Spike first: verify `applyConstraints` downscaling on a display-capture
track and on a transferred clone inside a worker (Chromium). Then:
`ladder.ts` gains `FramerateSelection` ('auto' | rung) + the joint-default
resolver (framerate-first: 60 if any rung probes HW at 60, else 30; never
'native') and the ceiling helper (slice `autoLadder` at the probed ceiling;
1080p software floor-ceiling); `transport/broadcaster.ts` computes the
resolved fps + ceiling from the injected matrix at start and on
fps/mode/codec changes, applies
sticky-target constraints on the capture track (via a new
`BroadcastMediaSource.applyConstraints?` seam method), and drops the
`acquireDisplayStream` fps cap; R11 worker command carries the sticky
target; preprocessor untouched as safety net.

| Acceptance criterion | Verified by |
|---|---|
| Ceiling = highest HW rung at effective fps; all-software matrix ⇒ 1080p ceiling | `ladder.test.ts` |
| 'auto' fps resolves framerate-first (60 when any rung probes HW at 60, else 30); never 'native'; all-software matrix ⇒ 30 | 〃 |
| Auto mode starts at the ceiling; up-probes stop there; explicit rungs bypass ceiling logic entirely | `broadcaster.test.ts` (fake matrix) |
| 'auto' fps never runtime-steps; it re-resolves only on a probe re-run (mode/codec/source change) | 〃 |
| Sticky-target change calls `applyConstraints` on the media source; auto steps do not | 〃 (fake source records calls) |
| `applyConstraints` rejection is swallowed (logged), pipeline keeps running, preprocessor still scales | 〃 |
| Worker path forwards the sticky target and applies constraints on the worker-side clone | `broadcast-worker-core.test.ts` |
| Spike outcome (clone constraints honored or not) recorded in this doc + README gotcha | doc update |

### L4 — settings store + advanced UI

`broadcastSettingsStore`: `hwPreference`, `bitrateOverride`,
`codecOverride`, and the framerate axis widened to `FramerateSelection`
with default 'auto' (persisted, validated on load like the rungs; a
previously persisted explicit fps rung keeps its exact meaning — only the
fresh-profile default changes).
`BroadcasterScreen`: "Advanced" disclosure in the settings panel (tri-state,
bitrate, codec); resolution/fps pickers annotated from the matrix
(badge/disabled + tooltip, live-recomputed on mode/codec change); overlay
Encode row (actual codec + HW/SW) on the production broadcaster surface.

| Acceptance criterion | Verified by |
|---|---|
| New store fields persist, validate, and default correctly (garbage localStorage falls back) | `broadcastSettingsStore.test.ts` |
| Framerate defaults to 'auto'; a persisted explicit rung (30/60/…) survives the widening unchanged | 〃 |
| Picker options render badges/disabled states from a fake matrix; annotations recompute on mode change | screen/component test |
| Overlay shows the Encode row; Copy-diagnostics JSON carries codec + acceleration | `BroadcasterStatsOverlay.test.tsx` |
| Settings changes mid-stream ride the existing session `setLadder`-style path (no restart) | `workerBroadcastSession.test.ts` / `broadcaster.test.ts` |

### L5 — docs + verification sync

ROADMAP R12 status, CLAUDE.md build-order + docs list, README gotchas
(prefer-hardware commitment; clone constraints are per-track and applied
worker-side; annotations are advisory — runtime acceleration is truth),
manual verification pass below.

| Acceptance criterion | Verified by |
|---|---|
| All gates green (`npm test`, lint, tsc, build; server untouched) | CI |
| Docs updated; gotchas landed in README | review |

## Verification plan (manual)

On the gaming PC (Chrome, NVENC): fresh profile → broadcaster surface →
default lands the highest HW rung at **60 fps** (NVENC probes HW at 60) and
the overlay Encode row says hardware; picker shows 4K@60 unmarked (HW) and
any SW-only combos badged. Switch to software-only → overlay says software,
auto resolves 1080p30, R4 step-down inducible under load (the R4
software-path scenario).
Hardware-only + an explicit rung the GPU can't do → start refused with the
clear error, no share picker shown (connect-before-picker preserved).
While streaming: change explicit rung → capture frames shrink (overlay
capture fps/dims + funnel rates confirm `applyConstraints` took effect, not
just the preprocessor), viewers ride through via the config re-emit path, no
restart. Bitrate override visibly moves sent bitrate; codec pin to VP9
negotiates VP9 end-to-end. On Firefox: all options badged SW, auto
resolves the 1080p30 software default, main-thread pipeline, streaming
works. Worker vs
main-thread: confirm constraints land on the right track on each path
(spike result).
