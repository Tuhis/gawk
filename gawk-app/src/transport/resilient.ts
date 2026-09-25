// Viewer delivery mode (live / resilient / deep): the module-scoped flag and
// the reorder-buffer profile constants that widen when it is not live. Like
// the playout mode (playout.ts), the flag lives in whichever JS context the
// pipeline runs (main thread, or the viewer worker via the 'resilient' worker
// command) and is read live — but unlike playout, flipping it mid-session is
// not a supported path: a mode change is a deliberate reconnect, because the
// delivery negotiation happens at subscribe time.
//
// This module holds only the raw flag + constants so reorder-buffer.ts can
// read them without import cycles; the public setter lives in playout.ts
// (setViewerDeliveryMode), which also resets the playout controller.

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

// The three points on the latency-for-smoothness axis. They really are one
// axis — each step buys more smoothness with more delay — so two booleans
// would make two controls out of one choice.
//
//   live       live-edge datagrams, no added delay (the default)
//   resilient  reliable carriers + the resilient adaptive buffer (~150-500 ms)
//   deep       the above, plus the relay's DVR ring and a multi-second buffer
//
// `deep` is a superset of `resilient`, which is what lets getResilientMode()
// stay the derived "not live-edge" signal every existing call site reads.
export type ViewerDeliveryMode = 'live' | 'resilient' | 'deep';

let mode: ViewerDeliveryMode = 'live';

// True for anything that is not live-edge: reliable carriers, the wider
// reorder profile, adaptive pacing.
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

// How many UNRECOVERED delta frames one GOP may skip before the viewer freezes
// to the next keyframe.
//
// Parity reduces how often a frame is unrecoverable; this bounds what one
// costs. A budget rather than a consecutive-run tolerance, because a budget is
// what the viewer can actually count, degrades predictably as loss rises, and
// caps artifact exposure per GOP at a number the operator chose.
//
// 0 is plain freeze-on-gap, which is what makes the behaviour revertible at
// runtime.
//
// Module state, like the playout mode: the pipeline reads it live per advance,
// and the worker receives it through the same command channel.
const DEFAULT_LOSS_ALLOWANCE_FRAMES = 1;
let lossAllowanceFrames = DEFAULT_LOSS_ALLOWANCE_FRAMES;

export function getLossAllowanceFrames(): number {
  return lossAllowanceFrames;
}

export function setLossAllowanceFrames(next: number): void {
  lossAllowanceFrames = Number.isFinite(next) && next > 0 ? Math.floor(next) : 0;
}
