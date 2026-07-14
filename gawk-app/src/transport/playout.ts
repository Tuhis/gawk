// Opt-in smoothed playout (R5 Q3, docs/15). Default OFF — the project's
// live-edge philosophy stands; this is a per-viewer latency-for-smoothness
// trade the viewer explicitly opts into, and the cost is visible (the stats
// overlay shows the mode and the latency rows rise by the offset).
//
// The setting is a module-scoped value in whichever JS context the pipeline
// runs (main thread, or the viewer worker via the 'playout' worker command),
// read live by the reorder buffer on every advance — so toggling mid-session
// re-paces without touching the pipeline.

// One value, a named tunable like KEYFRAME_WAIT_MS: ~9 frames at 60 fps,
// comfortably inside the reorder buffer's MAX_BUFFERED_FRAMES.
export const PLAYOUT_OFFSET_MS = 150;

let playoutOffsetMs = 0;

export function setSmoothedPlayout(on: boolean): void {
  playoutOffsetMs = on ? PLAYOUT_OFFSET_MS : 0;
}

export function getPlayoutOffsetMs(): number {
  return playoutOffsetMs;
}
