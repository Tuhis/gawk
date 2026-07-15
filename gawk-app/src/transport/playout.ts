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

let mode: PlayoutMode = 'off';

export function setPlayoutMode(m: PlayoutMode): void {
  mode = m;
}

export function getPlayoutMode(): PlayoutMode {
  return mode;
}

export function getPlayoutOffsetMs(): number {
  return mode === 'off' ? 0 : PLAYOUT_OFFSET_MS;
}
