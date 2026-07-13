# R3 — Broadcaster Resolution & Framerate Picker: Design + Implementation Plan

Design doc for [ROADMAP R3](../ROADMAP.md#r3--broadcaster-resolution--framerate-picker).
The broadcaster chooses what they send: resolution **native / 1080p / 720p /
480p**, framerate **native / 60 / 30 / 5 fps**. Native remains the default;
lower rungs trade fidelity for encode headroom and uplink bandwidth. The
picker is usable **live, mid-broadcast** — this is deliberate, because R4
(automatic fallback) will drive exactly the same mid-stream mechanism and R3
must prove it works.

## Goals

- Broadcaster-side ladder: pick resolution and framerate independently, full
  4×4 matrix, default native/native.
- Scaling and frame dropping happen **broadcaster-side, before encode**;
  capture stays at native resolution/rate so the ladder can change mid-stream
  without renegotiating `getDisplayMedia`.
- Mid-stream ladder changes: viewers recover automatically within one
  keyframe interval, late joiners after a change get a consistent
  config + keyframe pair from the relay cache.
- Bitrate follows the ladder — capping resolution/framerate actually reduces
  uplink usage (the 480p/5fps "sharing a menu" mode is pointless at a fixed
  6 Mbps).
- **Zero relay-server changes** — the relay stays a codec- and
  resolution-agnostic byte forwarder.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| H1 | Ladder + gate pure logic (`media/ladder.ts`, `FpsGate` in `media/preprocess.ts`) | Unit tests (written first) pass: target-size math (16:9 downscale, ultrawide, portrait, never-upscale, even-dimension rounding), fps-gate cadence (60→30, 60→5, jitter tolerance, stall recovery, native passthrough), bitrate policy (reference points + clamps) | ✅ done (2026-07-13; tests red-then-green) |
| H2 | Pipeline integration (`FramePreprocessor` scaler, time-based keyframe cadence in `encoder.ts`, encoder re-init on target change + `setLadder()` in `broadcaster.ts`) | Existing vitest suite still green; keyframe cadence unit test passes (time-based, not frame-count); `broadcaster.ts` compiles with preprocessor stage inserted and re-init path in place; loopback pipeline unaffected at native/native | ✅ done (2026-07-13) |
| H3 | Picker UI (`broadcastSettingsStore`, picker on BroadcastPage, stats rows) | Store persists rungs to localStorage; picker enabled while broadcasting; stats grid shows actual output resolution/fps/bitrate and fps-gate drops; lint + build green | ✅ done (2026-07-13) |
| H4 | Verification + docs sync | `npm test`, `npm run lint`, `npm run build` all green; manual browser matrix + mid-stream-change verify passed (see Manual Verification); README gotchas + ROADMAP + CLAUDE.md synced | ✅ done (manual browser verify passed 2026-07-13) |

Goal → verified-by, for the cross-cutting behaviors:

| Goal | Where | Verified by |
|------|-------|-------------|
| Rung → target size (AR-preserving, even, never upscale) | `ladder.computeTargetSize` | `ladder.test.ts` |
| Framerate limiting by timestamp gate | `preprocess.FpsGate` | `preprocess.test.ts` |
| Bitrate follows the ladder | `ladder.computeBitrate` | `ladder.test.ts` |
| Time-based keyframe cadence | `encoder.ts` (`keyframeIntervalMs`) | `encoder-keyframe.test.ts` |
| Mid-stream change → new config datagram precedes new keyframe | existing `broadcaster.ts` `handleEncoded` path (unchanged mechanism, new trigger) | manual verify: viewer recovers ≤ 1 keyframe interval after live rung change |
| Late joiner after a change gets consistent config+keyframe | existing relay cache (config cached on arrival, keyframe on full assembly) | manual verify: join a viewer after a mid-stream change, first picture is at the new resolution |

## Decisions

1. **Rung semantics — cap the longer dimension at the rung's 16:9 width
   equivalent (1080p → 1920, 720p → 1280, 480p → 854), preserve aspect
   ratio, never upscale.** If `max(width, height) > cap`, scale by
   `cap / max(width, height)`; otherwise pass through untouched. Target
   dimensions round to even (H.264 requirement) with a floor of 2.
   Ultrawide 3440×1440 at the 1080p rung becomes 1920×804 — no 16:9
   squeeze, just a proportional fit. **Why the longer dimension**: capping
   the shorter one (the first-draft rule, taken from the ROADMAP's
   "1080-height equivalents" example) leaves total pixel count unbounded —
   a hostile or misconfigured source shaped 25000×1080 would count as
   "already 1080p" and pass through at 27 megapixels. With the
   longer-dimension cap the worst case is cap² (a square source, ~3.7 MP at
   the 1080p rung). Selecting a rung at or above native is a passthrough —
   no upscaling, no re-encode penalty.
2. **Scaling path: main-thread `OffscreenCanvas` 2D `drawImage`.** There is
   no existing worker (the ROADMAP's "existing worker" was speculative) and
   moving the pipeline into one is a refactor R3 doesn't need: `drawImage`
   of a `VideoFrame` is GPU-backed in Chromium, so the main-thread cost is
   issuing the draw call, not the resample. One canvas + context is reused
   across frames and resized only when the target changes. The scaled
   `VideoFrame` is constructed from the canvas with the source frame's
   timestamp; the source frame is closed by the preprocessor.
   `createImageBitmap(frame, {resizeWidth…})` was considered and rejected:
   it is async (frame ordering into the encoder would need a chain) and
   allocates a bitmap per frame. Worker migration is a possible follow-up if
   profiling on the gaming PC shows main-thread jank.
3. **Framerate limiting: timestamp-based gate, before scaling.** A virtual
   schedule (`nextDueUs`) advanced by the target interval; frames arriving
   early are dropped, and a capture stall (gap > one interval) re-anchors
   the schedule so we never "catch up" by bursting. Dropping happens before
   the scaler so dropped frames cost nothing. Native rung = gate disabled.
   Gate decisions are pure (timestamps in, boolean out) and unit-tested.
4. **Mid-stream ladder change = encoder recreate, not `configure()`.**
   On a rung change (or a source-dimension change — same code path), the
   pipeline drops the current `Encoder` without flushing (in-flight frames
   are lost; drops-over-stalls) and re-enters the existing
   first-frame negotiation path with the next preprocessed frame. Recreating
   rather than reconfiguring: (a) guarantees the first output chunk carries
   fresh `decoderConfig` metadata — we don't have to rely on browsers
   re-emitting it after a live `configure()`; (b) reuses the probe cascade,
   so a rung the hardware encoder can't do falls back exactly like startup;
   (c) avoids old-resolution frames in the encoder queue being emitted after
   the config switch. `nextFrameId` is pipeline state and keeps counting —
   viewers' reassembler ordering is undisturbed. The existing
   `handleEncoded` already refreshes the config datagram from **any**
   chunk's `decoderConfig` metadata and prepends it to every keyframe, so
   the new config reaches the wire before the new keyframe with no new
   mechanism.
5. **Keyframe cadence becomes time-based**: `keyframeIntervalMs`
   (default 2000 ≈ today's 120 frames at 60 fps) replaces
   `keyframeIntervalFrames`. At 5 fps a 120-**frame** cadence is a 24-second
   GOP — a viewer recovering from loss would wait up to 24 s for the next
   keyframe. The encoder forces a keyframe when the frame timestamp is ≥
   `keyframeIntervalMs` past the last keyframe's; the first frame after
   (re)creation is always a keyframe.
6. **Bitrate policy**: `bitrate = 0.05 bits/pixel × width × height × 60 ×
   sqrt(fps/60)`, clamped to [0.5, 10] Mbps. Anchors: 1920×1080@60 ≈
   6.2 Mbps (today's fixed 6 Mbps), 1280×720@60 ≈ 2.8 Mbps, 1080p@5 ≈
   1.8 Mbps (sqrt keeps static-content keyframes crisp instead of scaling
   linearly to 0.5 Mbps), 4K native@60 clamps to 10 Mbps. 15 viewers × 10
   Mbps = 150 Mbps worst case — comfortable on the 1 gbps uplink and under
   the R2 egress cap's discretion.
7. **Full 4×4 matrix allowed.** 480p@60 is a legitimate "competitive
   twitch gameplay on a bad uplink" combination; constraining the matrix
   buys nothing for a 15-friend audience.
8. **The relay's momentary cache inconsistency is accepted.** The relay
   caches the config datagram on arrival but the keyframe only once fully
   assembled, so after a mid-stream change there is a sub-frame-time window
   where a joining viewer is primed with the new config + the old-resolution
   keyframe. The decoder either decodes it scaled (VP8/VP9 handle
   per-frame sizes) or errors → `ViewerSession` reconnects → re-primed
   consistently. Accepted per drops-over-stalls; no relay change.
9. **Picker lives on the broadcast page, enabled while live; selection
   persists in localStorage** (new `broadcastSettingsStore`, same pattern as
   `transportStore`). The loopback page is untouched: its decoder applies
   `decoderConfig` only on the first encoded chunk, so live ladder changes
   there would need its decode half reworked — out of R3 scope (R6 decides
   the debug surface's future anyway).
10. **Framerate changes also recreate the encoder.** The encoder's
    `framerate` and `bitrate` config drive rate control; a 60→5 change with
    a stale 60 fps/6 Mbps config would misallocate bits badly. Rung changes
    of either axis go through the same recreate path (simpler than deciding
    per-axis, and R4 will step both).

## Why the viewer and relay need no changes

Walking the existing machinery for a mid-stream change from 1080p to 720p:

1. Broadcaster: `setLadder()` updates the preprocessor target and flags an
   encoder reset. The next captured frame is gated/scaled to 720p; the
   pipeline sees the pending reset, disposes the encoder, and re-runs
   first-frame negotiation at 1232×720 (say). The new encoder's first chunk
   is a keyframe carrying fresh `decoderConfig` metadata.
2. `handleEncoded` rebuilds `configDatagram` from that metadata and — as for
   every keyframe — sends it immediately before the keyframe's chunk 0.
3. Relay: `relayConfig` replaces the cached config and fans it out;
   the keyframe assembles and replaces the cached keyframe. Connected
   viewers get config-then-keyframe in order; new joiners get the cached
   pair.
4. Viewer: the reassembler dedupes config datagrams by **byte equality** —
   the changed config differs, so it passes through; `applyConfig` sets
   `waitingForKeyframe`, chains `decoder.configure()`, and the next
   keyframe decodes at the new size. Old-resolution delta frames still in
   flight are discarded by the keyframe gate. `ViewPage`'s canvas already
   sizes itself per decoded frame.

Every step is existing, shipped behavior; R3 only adds the trigger. The
switch-back case (1080→720→1080) also works: dedupe compares against the
*last* config, not a set.

## Proposed Changes

### `gawk-app`

#### [NEW] src/media/ladder.ts

Rung definitions and pure math. `RESOLUTION_RUNGS = ['native', 1080, 720,
480]`, `FRAMERATE_RUNGS = ['native', 60, 30, 5]` (types
`ResolutionRung = 'native' | number`, same for framerate).
`computeTargetSize(srcW, srcH, rung) → {width, height} | null` (null =
passthrough) per Decision 1. `computeBitrate(width, height, fps) → number`
per Decision 6 (native fps passed as the measured/assumed source rate,
default 60 when unknown).

#### [NEW] src/media/preprocess.ts

- `FpsGate` — pure timestamp gate per Decision 3: `accept(timestampUs) →
  boolean`, `setTargetFps(fps | null)`, drop counter. No DOM/WebCodecs
  dependencies.
- `FramePreprocessor` — owns an `FpsGate` + lazily-created
  `OffscreenCanvas`; `process(frame) → VideoFrame | null`. Returns the input
  frame unchanged when no scaling applies (passthrough must stay zero-copy);
  otherwise draws into the canvas and returns a new `VideoFrame` with the
  source timestamp, closing the source. `setTarget(resolutionRung, fpsRung)`
  takes effect on the next frame. Exposes `{gateDropped, scaledFrames,
  outputWidth, outputHeight}` stats.

#### [NEW] src/media/ladder.test.ts, src/media/preprocess.test.ts

Unit tests, written before the implementations (CODE-REVIEW.md discipline
applies to features' pure cores too): target-size cases (4K→1080p,
ultrawide 3440×1440→2580×1080, portrait, at-native passthrough,
below-native passthrough, odd-dimension rounding), gate cadence cases
(exact 60→30 alternation, 60→5, jittered input, stall re-anchor, native
passthrough), bitrate anchors and clamps. `FramePreprocessor`'s canvas half
is not unit-testable in jsdom — covered by manual verification.

#### [MODIFY] src/media/types.ts

`CaptureConfig`: `keyframeIntervalFrames` → `keyframeIntervalMs` (default
2000). No other shape changes — width/height/bitrate/framerate stay, they
are now *computed* by the broadcaster pipeline from ladder + actual frame.

#### [MODIFY] src/media/encoder.ts

- Keyframe decision becomes time-based per Decision 5 (track last-keyframe
  timestamp from the frames' own timestamps).
- `EncoderConfigured` gains `width`, `height`, `framerate`, `bitrate` so the
  UI can show what's actually being sent.
- `dispose()`: close without flush, for the recreate path.
- Extract the keyframe-cadence decision into a small pure helper so it's
  unit-testable (`encoder-keyframe.test.ts`).

#### [MODIFY] src/media/loopback.ts

Field rename only (`keyframeIntervalMs`). No preprocessor here (Decision 9).

#### [MODIFY] src/transport/broadcaster.ts

- Insert `FramePreprocessor` between capture and encoder in the frame
  callback: gate → scale → (maybe re-init encoder) → encode.
- Lift the `encoderInitStarted` closure flag into a `resetEncoder()` path:
  on ladder change or when a preprocessed frame's dimensions differ from
  the encoder's configured size, dispose the encoder and let the existing
  first-frame negotiation re-run with the current frame. (This incidentally
  covers mid-capture source dimension changes, previously unhandled.)
- `setLadder(resolutionRung, framerateRung)` public method: updates the
  preprocessor + flags the reset; callable any time while running.
- Encoder config computed from the *preprocessed* frame (project rule:
  trust the frame in hand) + `computeBitrate`.
- `BroadcastStats` gains `fpsGateDropped` and current output
  width/height/targetFps/bitrate.

#### [NEW] src/state/broadcastSettingsStore.ts

Zustand store persisting `resolutionRung` / `framerateRung` to localStorage
(`gawk.resolutionRung`, `gawk.framerateRung`), defaults `'native'`.
Same pattern as `transportStore`.

#### [MODIFY] src/features/stream/BroadcastPage.tsx (+ stream.module.css)

Two labeled `<select>`s (resolution, framerate) next to the controls.
Enabled both idle and live; while live, changes call
`pipeline.setLadder(...)`. Stats grid gains "Sending" (actual output
`W×H @ fps`, bitrate) and "Dropped (fps gate)" rows.

### `gawk-server`

No changes (see "Why the viewer and relay need no changes"). The relay
never parses resolution; datagram sizes only shrink at lower rungs.

## Verification Plan

### Automated Tests

- `ladder.test.ts` — target-size and bitrate cases listed above.
- `preprocess.test.ts` — `FpsGate` cadence cases listed above.
- `encoder-keyframe.test.ts` — time-based cadence: 60 fps input yields a
  keyframe every ~120 frames, 5 fps input every ~10 frames, first frame
  always key.
- Existing suites (`broadcaster.test.ts`, `viewer*.test.ts`,
  `wire.test.ts`, `packetize-reassemble.test.ts`, `ViewPage.test.tsx`) stay
  green — the wire format and viewer are untouched.

### Manual Verification

On the real setup (Chromium broadcaster, ≥1 Chromium + 1 Firefox viewer):

1. **Matrix spot-checks**: native/native (behavior identical to pre-R3),
   1080p/60, 720p/30, 480p/5. Confirm the stats grid's "Sending" row matches
   the picker and the viewer's decoded canvas is the expected size.
2. **Mid-stream resolution change** (1080p → 720p → 1080p while two viewers
   watch): picture recovers at each step within ~1 keyframe interval
   (≤ 2 s), no viewer reconnect needed, `Configs applied` increments on the
   viewers, stream continues.
3. **Mid-stream framerate change** (60 → 5 → 60): viewer fps follows,
   keyframes keep arriving ~every 2 s at 5 fps (time-based cadence), uplink
   `Sent` rate drops accordingly.
4. **Late joiner after a change**: change rungs, then open a new viewer —
   first picture is at the new resolution (relay cache consistency).
5. **Ultrawide/odd source** (window share with odd dimensions): scaled
   dimensions are even, aspect ratio preserved.
6. **5 fps GOP check**: at ×/5, `Keyframes` on the broadcaster advances
   ~every 2 s, not every 24 s.

## Implementation notes (2026-07-13)

- Implemented as designed; one post-implementation correction: Decision 1
  originally capped the *shorter* dimension (following the ROADMAP's
  ultrawide example), which review caught as leaving pixel count unbounded
  (25000×1080 would pass as "1080p"). Reworked test-first to the
  longer-dimension cap now described above; the ROADMAP example was synced.
- The encoder-reset trigger is twofold and converges on one path: an
  explicit `setLadder()` flags `pendingEncoderReset`; independently, each
  preprocessed frame's even-rounded `displayWidth/Height` is compared with
  the live encoder's configured size, so mid-capture **source** dimension
  changes (previously unhandled) now also renegotiate cleanly.
- `Encoder.dispose()` (close without flush + late-callback suppression)
  exists so a dying encoder can't fail the pipeline that already replaced
  it; `close()` (flushing) remains the orderly-shutdown path.
- The `getSettings().frameRate` value survives only as the rate-control
  seed when the framerate rung is `native`; every dimension still comes
  from the actual `VideoFrame` (docs/01 rule).
- Automated gates green: 92 vitest tests (31 new: 19 ladder + 8 fps gate +
  4 keyframe cadence), oxlint, `tsc -b` + vite build.
- **Manual browser verification (steps 1–6 above) not yet run** — needs the
  real broadcaster/viewer setup; the status table flips H4 to done when it
  passes.
