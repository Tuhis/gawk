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

let resilient = false;

export function getResilientMode(): boolean {
  return resilient;
}

// Internal: flips the raw flag only. Callers outside playout.ts must use
// playout.ts's setResilientMode, which also resets the playout controller
// onto the right profile.
export function setResilientModeFlag(on: boolean): void {
  resilient = on;
}
