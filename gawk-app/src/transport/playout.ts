// Playout modes (R5 Q3 + R12 T2, docs/15 + docs/17). Default OFF — the
// project's live-edge philosophy stands; both smoothing modes are per-viewer
// latency-for-smoothness trades the viewer explicitly opts into, and the cost
// is visible (the stats overlay shows the mode and the latency rows rise by
// the offset).
//
//   'off'      — live-edge: release + present immediately (the default).
//   'fixed'    — R5 Q3's constant 150 ms decoder-release pacing, preserved
//                exactly as it shipped.
//   'adaptive' — R12: sub-frame paced presentation with a jitter-tracked
//                offset (T3; until then the seed constant) and a decode lead.
//
// R19 (docs/24 Decision 7): while Resilient mode is on, the *effective* mode
// is 'adaptive' with a wider controller profile ([150, 2000] ms, seed 500) —
// the stored mode keeps its value and its semantics and regains effect the
// moment Resilient mode turns off.
//
// The setting is a module-scoped value in whichever JS context the pipeline
// runs (main thread, or the viewer worker via the 'playout' worker command),
// read live by the reorder buffer on every advance — so switching mode
// mid-session re-paces without touching the pipeline.

import { getDvrBufferMs } from '../config';
import { LIVE_EDGE_WINDOW_MS, QUANTILE_RANGE_MS } from './live-edge';
import {
  getDeepBuffer,
  getResilientMode,
  getViewerDeliveryMode,
  setViewerDeliveryModeFlag,
  type ViewerDeliveryMode,
} from './resilient';

export type PlayoutMode = 'off' | 'fixed' | 'adaptive';

// One value, a named tunable like KEYFRAME_WAIT_MS: ~9 frames at 60 fps,
// comfortably inside the reorder buffer's MAX_BUFFERED_FRAMES. Fixed mode's
// constant, and adaptive mode's seed until the T3 controller takes over.
export const PLAYOUT_OFFSET_MS = 150;

// R12 T2 (docs/17 Decision 4): in adaptive mode, frames release from the
// reorder buffer this much before their display target so decoded frames
// reach the presentation sink just in time — the pre-decode pace stays the
// decoder frame-pool bound, the sink does the final ±½-vsync alignment.
// ~1 frame interval at 30 fps; T6 re-sizes it from measured decode jitter.
export const DECODE_LEAD_MS = 35;

// R12 T3 (docs/17 Decision 6): the adaptive offset controller. Target =
// clamp(arrival jitter (p95 − min) + headroom, [min, max]); the current
// offset slews toward it asymmetrically — up fast (under-buffering means
// visible drops NOW), down slowly and only after the target has sat well
// below the current value for a dwell period (fallback.ts's step-down-fast /
// probe-up-slow philosophy). Slew, not step: the paced sink turns offset
// changes directly into presentation cadence, so a slewed offset is an
// invisible fractional playback-rate nudge where a step would be a skip.
export const HEADROOM_MS = 34; // one 30 fps interval over the p95
export const MIN_PLAYOUT_OFFSET_MS = 50;
export const MAX_PLAYOUT_OFFSET_MS = 350; // inside MAX_BUFFERED_FRAMES, under KEYFRAME_WAIT_MS
export const OFFSET_SLEW_UP_MS_PER_S = 50;
export const OFFSET_SLEW_DOWN_MS_PER_S = 5;
export const OFFSET_DOWN_MARGIN_MS = 30; // target must sit this far below to arm a descent
export const OFFSET_DOWN_DWELL_MS = 15_000;
export const OFFSET_WARMUP_MS = 5000; // seed holds until the jitter window has data

// The clamp/seed/slew envelope of the adaptive controller (R19 made it a
// profile). The formula is shared; only these numbers widen in resilient
// mode — retransmit stalls inflate arrival p95, so the offset grows exactly
// when loss is happening and shrinks (slowly, dwell-gated) when the link
// cleans up.
//
// A profile also owns the *measurement* feeding it (R19 hardening,
// PLAYOUT-1): the arrival-jitter estimator is a fixed-range histogram, so a
// clamp the histogram cannot express is dead envelope. docs/24 Decision 7
// assumed "the existing WindowedQuantileTracker needs no changes" and it was
// wrong — its 500 ms range pinned the resilient offset at ~534 ms, less than
// the retransmit stalls the mode exists to absorb. Keep
// `quantileRangeMs >= maxMs` in every profile.
export interface PlayoutProfile {
  seedMs: number;
  minMs: number;
  maxMs: number;
  slewUpMsPerS: number;
  slewDownMsPerS: number;
  // Range of the arrival-jitter histogram (reorder-buffer.ts). Values past it
  // clamp into the top bin, so this is the largest jitter the profile can see.
  quantileRangeMs: number;
  // Sliding window the p95 is taken over. Shorter = faster reaction in both
  // directions; the *down* direction is deliberately governed by the dwell +
  // slew below, not by how fast a bad episode ages out of the window.
  jitterWindowMs: number;
  // A rise larger than this steps the offset straight to target instead of
  // slewing. Infinity = always slew (the default profile's shipped behavior).
  stepUpAboveMs: number;
}

export const DEFAULT_PLAYOUT_PROFILE: PlayoutProfile = {
  seedMs: PLAYOUT_OFFSET_MS,
  minMs: MIN_PLAYOUT_OFFSET_MS,
  maxMs: MAX_PLAYOUT_OFFSET_MS,
  slewUpMsPerS: OFFSET_SLEW_UP_MS_PER_S,
  slewDownMsPerS: OFFSET_SLEW_DOWN_MS_PER_S,
  quantileRangeMs: QUANTILE_RANGE_MS,
  jitterWindowMs: LIVE_EDGE_WINDOW_MS,
  stepUpAboveMs: Infinity,
};

// R19 resilient profile (docs/24 Decision 7): clamp [150, 2000] ms, seed 500,
// slew up 100 ms/s / down 10 ms/s. Provisional until X6's measurement pass.
export const RESILIENT_PLAYOUT_PROFILE: PlayoutProfile = {
  seedMs: 500,
  minMs: 150,
  maxMs: 2000,
  slewUpMsPerS: 100,
  slewDownMsPerS: 10,
  // 2500 > maxMs: p95 stays honest right up to the top of the clamp instead
  // of saturating against the histogram wall.
  quantileRangeMs: 2500,
  // 8 s, not 60: a handover spike is a seconds-scale event, and at 60 s it
  // sits under the p95 of a minute of clean samples and barely moves the
  // offset (PLAYOUT-3). The min tracker keeps its 60 s window — it is also
  // the release-schedule anchor, and offset ≈ p95 − min₆₀ is what makes
  // `timestamp + min₆₀ + offset` land at the measured p95.
  jitterWindowMs: 8000,
  // Slewing 1 s of new buffer in at 100 ms/s costs 10 s of the stutter this
  // mode exists to remove, so a large rise is taken at once: the alternative
  // to a one-time pause is frames missing their slot and freezing to the next
  // keyframe. Small rises still slew (invisible), and *down* is never stepped.
  stepUpAboveMs: 150,
};

// R21 (docs/26): the buffer a viewer requests from the relay, and the floor it
// applies once the relay confirms it is serving from a ring. Deliberately NOT
// applied on request: against a relay that cannot honour it, a deep buffer is
// pure latency for no benefit, so the floor only deepens on a DeliveryAck
// saying `dvr`. The value must strictly exceed the stall it covers — 3 s of
// buffer backs a ~2 s stall at 3x recovery bandwidth (docs/26 Decision 6).
export const DVR_BUFFER_MS = getDvrBufferMs();

// R21 DVR profile: the resilient envelope with its floor raised to what the
// relay is now able to keep filled. maxMs rises with it so the adaptive
// controller is not clamped below its own floor.
export const DVR_PLAYOUT_PROFILE: PlayoutProfile = {
  ...RESILIENT_PLAYOUT_PROFILE,
  seedMs: DVR_BUFFER_MS,
  minMs: DVR_BUFFER_MS,
  maxMs: Math.max(RESILIENT_PLAYOUT_PROFILE.maxMs, DVR_BUFFER_MS),
  quantileRangeMs: Math.max(RESILIENT_PLAYOUT_PROFILE.quantileRangeMs, DVR_BUFFER_MS + 500),
};

// What the relay's DeliveryAck said about this session, or null before it
// arrives. Module state like the delivery mode itself, and reset on every mode
// change — a new session must re-establish it rather than inherit it from a
// relay that may have been replaced mid-view.
//
// Deliberately three-valued rather than a boolean, and the deep profile
// applies while it is still null (docs/26 Decision 7, revised 2026-07-23).
// The original rule was "never deepen on request, only on grant", to avoid
// paying latency a relay could not back. But the two directions are not
// symmetric: DEEPENING mid-session makes the reorder buffer hold frames
// longer, which is a visible multi-second freeze while it refills — the E2E
// deep-buffer pass caught exactly that, ~2 s of frozen video at startup,
// indistinguishable to a viewer from the bug it was written to catch.
// SHORTENING costs nothing: frames simply become due sooner. So the buffer a
// user asked for applies immediately, and a denial shortens it.
type DvrAck = 'granted' | 'denied';
let dvrAck: DvrAck | null = null;

export function setDvrGranted(granted: boolean): void {
  dvrAck = granted ? 'granted' : 'denied';
}

export function getDvrGranted(): boolean {
  return dvrAck === 'granted';
}

// Clears the ack for a new session (a mode change is a deliberate reconnect).
export function resetDvrAck(): void {
  dvrAck = null;
}

// The profile in force in this JS context.
export function getPlayoutProfile(): PlayoutProfile {
  if (!getResilientMode()) return DEFAULT_PLAYOUT_PROFILE;
  // The user's choice applies at once; only an explicit denial walks it back.
  // See the DvrAck comment for why the asymmetry matters.
  return getDeepBuffer() && dvrAck !== 'denied' ? DVR_PLAYOUT_PROFILE : RESILIENT_PLAYOUT_PROFILE;
}

export class PlayoutController {
  private profile: () => PlayoutProfile;
  private current: number;
  private firstJitterAt: number | null = null;
  private lastUpdateAt: number | null = null;
  private belowSince: number | null = null;

  // The profile is a live getter so the module singleton follows the
  // resilient flag; tests may pass a fixed profile.
  constructor(profile: PlayoutProfile | (() => PlayoutProfile) = DEFAULT_PLAYOUT_PROFILE) {
    this.profile = typeof profile === 'function' ? profile : () => profile;
    this.current = this.profile().seedMs;
  }

  // Feed the current arrival jitter (null = no data yet). Cadence is the
  // caller's (the pipeline's stats tick); slew is rate × dt, so the exact
  // cadence doesn't matter.
  update(jitterMs: number | null, nowMs: number): void {
    const dtMs = this.lastUpdateAt === null ? 0 : Math.max(0, nowMs - this.lastUpdateAt);
    this.lastUpdateAt = nowMs;
    if (jitterMs === null) return;
    if (this.firstJitterAt === null) this.firstJitterAt = nowMs;
    if (nowMs - this.firstJitterAt < OFFSET_WARMUP_MS) return;

    const p = this.profile();
    const target = Math.min(p.maxMs, Math.max(p.minMs, jitterMs + HEADROOM_MS));
    if (target > this.current) {
      this.belowSince = null;
      this.current =
        target - this.current > p.stepUpAboveMs
          ? target
          : Math.min(target, this.current + (p.slewUpMsPerS * dtMs) / 1000);
    } else if (target < this.current) {
      // Arm a descent only on a clear gap; once armed, keep descending all
      // the way to the target even as the gap narrows below the margin —
      // otherwise the offset would floor at target + margin forever.
      if (this.belowSince === null && target < this.current - OFFSET_DOWN_MARGIN_MS) {
        this.belowSince = nowMs;
      }
      if (this.belowSince !== null && nowMs - this.belowSince >= OFFSET_DOWN_DWELL_MS) {
        this.current = Math.max(target, this.current - (p.slewDownMsPerS * dtMs) / 1000);
      }
    } else {
      this.belowSince = null;
    }
  }

  offsetMs(): number {
    return this.current;
  }

  // Broadcaster restart / mode re-entry: new timestamp timeline, stale jitter.
  reset(): void {
    this.current = this.profile().seedMs;
    this.firstJitterAt = null;
    this.lastUpdateAt = null;
    this.belowSince = null;
  }
}

let mode: PlayoutMode = 'off';
const controller = new PlayoutController(getPlayoutProfile);

export function setPlayoutMode(m: PlayoutMode): void {
  // Entering adaptive re-seeds: jitter observed under another mode's pacing
  // (or a previous session) shouldn't preload the controller. While resilient
  // mode is live the controller belongs to it — a stored-mode change must not
  // reset the running resilient offset (it has no live effect anyway).
  if (m === 'adaptive' && mode !== 'adaptive' && !getResilientMode()) controller.reset();
  mode = m;
}

// The stored playout mode, as toggled by the user (docs/24 Decision 7 keeps
// its semantics untouched while resilient mode overrides the effective mode).
export function getStoredPlayoutMode(): PlayoutMode {
  return mode;
}

// The effective mode: resilient mode implies adaptive pacing with the
// resilient profile while active.
export function getPlayoutMode(): PlayoutMode {
  return getResilientMode() ? 'adaptive' : mode;
}

export function getPlayoutOffsetMs(): number {
  const m = getPlayoutMode();
  if (m === 'off') return 0;
  if (m === 'fixed') return PLAYOUT_OFFSET_MS;
  return controller.offsetMs();
}

// Driven by the pipeline's stats tick with the reorder buffer's arrival
// jitter; no-op outside (effective) adaptive mode.
export function updatePlayoutController(jitterMs: number | null, nowMs: number): void {
  if (getPlayoutMode() !== 'adaptive') return;
  controller.update(jitterMs, nowMs);
}

// Broadcaster restart: the arrival-jitter window resets with the baseline,
// and so must the controller reading it.
export function resetPlayoutController(): void {
  controller.reset();
}

// R19 (docs/24 Decision 9): the one entry point for flipping resilient mode
// in this JS context. Resets the controller across the flip so the offset
// re-seeds on the incoming profile (500 ms entering resilient, 150 ms
// leaving it) instead of carrying a value from the other profile's envelope.
export function setViewerDeliveryMode(next: ViewerDeliveryMode): void {
  if (next === getViewerDeliveryMode()) return;
  setViewerDeliveryModeFlag(next);
  // Reset across every step, not just across the live boundary: resilient and
  // deep have different envelopes, so a target learned under one would be
  // carried into the other's clamp (the R19 rule, extended to three states).
  controller.reset();
  // A mode change is a deliberate reconnect, so the previous session's ack
  // says nothing about the next one — and "unknown" is not "denied".
  resetDvrAck();
}
