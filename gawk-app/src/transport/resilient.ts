// Resilient viewer mode (R19, docs/24): the module-scoped flag and the
// reorder-buffer profile constants that widen when it is on. Like the playout
// mode (playout.ts), the flag lives in whichever JS context the pipeline runs
// (main thread, or the viewer worker via the 'resilient' worker command) and
// is read live — but unlike playout, flipping it mid-session is not a
// supported path: a mode change is a deliberate reconnect (docs/24
// Decision 9), because the delivery negotiation happens at subscribe time.
//
// This module holds only the raw flag + constants so reorder-buffer.ts can
// read them without import cycles; the public setter lives in playout.ts
// (setResilientMode), which also swaps the playout controller profile.

// Provisional values per docs/24 Decision 7, to be confirmed or amended by
// the X6 measurement pass.

// A 2 s budget at 60 fps is 120 frames before headroom; these are *encoded*
// frames, so memory stays trivial (~2 MB at 8 Mbps for a full 2 s).
export const RESILIENT_MAX_BUFFERED_FRAMES = 256;
// Within one carrier, records arrive in order — a missing predecessor on the
// same carrier is genuinely gone. But across a rotation the draining
// predecessor carrier can trail the new one by a retransmit (~RTT), so
// cross-carrier stragglers deserve RTT-scale patience.
export const RESILIENT_DELTA_GAP_GRACE_MS = 250;
// A ~236 KB store-and-forwarded keyframe on a throttled link deserves the
// same patience the rest of the budget gets.
export const RESILIENT_KEYFRAME_WAIT_MS = 2000;

// R21 (docs/26 Decision 15): the three points on the latency-for-smoothness
// axis, replacing R19's boolean. They really are one axis — each step buys
// more smoothness with more delay — so a boolean plus a second boolean would
// have made two controls out of one choice.
//
//   live       live-edge datagrams, no added delay (the default)
//   resilient  reliable carriers + the R19 adaptive buffer (~150-500 ms)
//   deep       the above, plus the R21 relay ring and a multi-second buffer
//
// `deep` is a superset of `resilient`, which is what lets getResilientMode()
// stay the derived "not live-edge" signal every existing call site reads.
export type ViewerDeliveryMode = 'live' | 'resilient' | 'deep';

let mode: ViewerDeliveryMode = 'live';

// True for anything that is not live-edge: reliable carriers, the wider
// reorder profile, adaptive pacing. Deliberately unchanged in meaning from
// R19, so reorder-buffer.ts and playout.ts read it exactly as before.
export function getResilientMode(): boolean {
  return mode !== 'live';
}

// True only for the deep-buffer step: whether to ask the relay for a DVR ring
// and, once granted, hold the multi-second floor.
export function getDeepBuffer(): boolean {
  return mode === 'deep';
}

export function getViewerDeliveryMode(): ViewerDeliveryMode {
  return mode;
}

// Internal: sets the raw mode only. Callers outside playout.ts must use
// playout.ts's setViewerDeliveryMode, which also resets the playout controller
// onto the right profile.
export function setViewerDeliveryModeFlag(next: ViewerDeliveryMode): void {
  mode = next;
}
