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
// The setting is a module-scoped value in whichever JS context the pipeline
// runs (main thread, or the viewer worker via the 'playout' worker command),
// read live by the reorder buffer on every advance — so switching mode
// mid-session re-paces without touching the pipeline.

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
// clamp(arrival jitter (p95 − min) + headroom, [MIN, MAX]); the current
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

export class PlayoutController {
  private current = PLAYOUT_OFFSET_MS;
  private firstJitterAt: number | null = null;
  private lastUpdateAt: number | null = null;
  private belowSince: number | null = null;

  // Feed the current arrival jitter (null = no data yet). Cadence is the
  // caller's (the pipeline's stats tick); slew is rate × dt, so the exact
  // cadence doesn't matter.
  update(jitterMs: number | null, nowMs: number): void {
    const dtMs = this.lastUpdateAt === null ? 0 : Math.max(0, nowMs - this.lastUpdateAt);
    this.lastUpdateAt = nowMs;
    if (jitterMs === null) return;
    if (this.firstJitterAt === null) this.firstJitterAt = nowMs;
    if (nowMs - this.firstJitterAt < OFFSET_WARMUP_MS) return;

    const target = Math.min(
      MAX_PLAYOUT_OFFSET_MS,
      Math.max(MIN_PLAYOUT_OFFSET_MS, jitterMs + HEADROOM_MS),
    );
    if (target > this.current) {
      this.belowSince = null;
      this.current = Math.min(target, this.current + (OFFSET_SLEW_UP_MS_PER_S * dtMs) / 1000);
    } else if (target < this.current) {
      // Arm a descent only on a clear gap; once armed, keep descending all
      // the way to the target even as the gap narrows below the margin —
      // otherwise the offset would floor at target + margin forever.
      if (this.belowSince === null && target < this.current - OFFSET_DOWN_MARGIN_MS) {
        this.belowSince = nowMs;
      }
      if (this.belowSince !== null && nowMs - this.belowSince >= OFFSET_DOWN_DWELL_MS) {
        this.current = Math.max(target, this.current - (OFFSET_SLEW_DOWN_MS_PER_S * dtMs) / 1000);
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
    this.current = PLAYOUT_OFFSET_MS;
    this.firstJitterAt = null;
    this.lastUpdateAt = null;
    this.belowSince = null;
  }
}

let mode: PlayoutMode = 'off';
const controller = new PlayoutController();

export function setPlayoutMode(m: PlayoutMode): void {
  // Entering adaptive re-seeds: jitter observed under another mode's pacing
  // (or a previous session) shouldn't preload the controller.
  if (m === 'adaptive' && mode !== 'adaptive') controller.reset();
  mode = m;
}

export function getPlayoutMode(): PlayoutMode {
  return mode;
}

export function getPlayoutOffsetMs(): number {
  if (mode === 'off') return 0;
  if (mode === 'fixed') return PLAYOUT_OFFSET_MS;
  return controller.offsetMs();
}

// Driven by the pipeline's stats tick with the reorder buffer's arrival
// jitter; no-op outside adaptive mode.
export function updatePlayoutController(jitterMs: number | null, nowMs: number): void {
  if (mode !== 'adaptive') return;
  controller.update(jitterMs, nowMs);
}

// Broadcaster restart: the arrival-jitter window resets with the baseline,
// and so must the controller reading it.
export function resetPlayoutController(): void {
  controller.reset();
}
