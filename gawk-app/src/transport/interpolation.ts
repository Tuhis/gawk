// R12 T4 (docs/17 Decision 7): frame interpolation — EXPERIMENTAL, its own
// default-off opt-in, and pre-registered for removal if it fails the kill
// criteria. The scaffold synthesizes a mid frame between consecutive
// presented frames (30 → 60 fps) with a linear blend; T5 replaces the blend
// with motion-estimated warping. Ghosting on the blend is the expected
// scaffold outcome — it validates the two-texture pipeline and this slot
// scheduling, not the final look.
//
// Interpolation requires adaptive (paced) playout: pacing owns the display
// slots that the synthesized frame slots between. It is opportunistic — a
// mid frame is synthesized only when the NEXT real frame is already in hand
// (the decode lead + adaptive offset make that the common case), so it adds
// no latency of its own; when the next frame isn't there yet, the interval
// simply isn't interpolated.
//
// Like the playout mode, the toggle is module state per JS context, crossed
// into the viewer worker via the 'interpolation' command and read live.

let enabled = false;

export function setInterpolationEnabled(on: boolean): void {
  enabled = on;
}

export function getInterpolationEnabled(): boolean {
  return enabled;
}

// Don't synthesize across a stall, drop, or resync: blending two frames a
// long gap apart hallucinates content. ~3 source intervals at 30 fps.
export const MAX_INTERPOLATION_GAP_MS = 100;

// The synthesized frame's display slot between two consecutive presented
// targets, or null when there is nothing sane to synthesize (no previous
// frame, non-monotonic targets, or a gap too wide to blend across).
export function midSlotMs(
  prevTargetMs: number | null,
  nextTargetMs: number,
  maxGapMs = MAX_INTERPOLATION_GAP_MS,
): number | null {
  if (prevTargetMs === null) return null;
  const gap = nextTargetMs - prevTargetMs;
  if (gap <= 0 || gap > maxGapMs) return null;
  return prevTargetMs + gap / 2;
}
