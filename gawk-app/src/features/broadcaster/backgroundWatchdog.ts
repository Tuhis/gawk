// The hidden-tab watchdog (docs/44 §4.8 revision 2026-09-06, docs/06 the
// same day). A broadcaster tab left in the background keeps its relay
// session alive — the browser's network process answers the QUIC keepalives
// — while, on the main-thread capture path, the frames stop: measured as a
// slot held and a room tile marked live for five hours on ~50 s of video.
//
// The relay cannot tell that tab from a paused game on a static screen:
// capture is damage-driven, so both deliver no video (docs/19, docs/28,
// viewer.ts). The page can: it knows it is hidden. So the page ends its
// own broadcast after BACKGROUND_STOP_MS hidden with no frame encoded in
// that span, and says so on the card when the tab comes back.
//
// What it does NOT catch, by design: a hidden tab whose frames keep flowing
// (the worker-offload path on Windows keeps encoding in the background —
// that stream is fine and stays), and a visible static screen (never
// hidden, never stopped — docs/30 §7).
//
// What it DOES catch beyond the background tab: a fully occluded window on
// macOS, which Chrome also reports as hidden and where the main-thread path
// already stops the frames (docs/16). A fullscreen game covering the browser
// for five minutes therefore ends what was a frozen stream; within the
// broadcast grace a restart reclaims the same code.

export const BACKGROUND_STOP_MS = 5 * 60 * 1000;

// "Hidden or covered": on macOS Chrome reports a fully occluded window as
// hidden too — a fullscreen game over the browser — and on the main-thread
// capture path that already freezes the stream, so the stop applies there as
// well (review of PR #302). Within the broadcast grace a restart reclaims
// the same code.
export const BACKGROUND_STOP_NOTE =
  'Stopped: this window was hidden or covered for 5 minutes with no video. Start again when you’re back — within a few minutes you keep the same code.';

// One sample per stats tick. Returns true exactly once, on the tick that
// crosses the threshold; the caller stops the broadcast.
export class BackgroundWatchdog {
  private hiddenSince: number | null = null;
  private framesAtHidden = 0;
  private fired = false;
  private readonly thresholdMs: number;

  constructor(thresholdMs: number = BACKGROUND_STOP_MS) {
    this.thresholdMs = thresholdMs;
  }

  sample(hidden: boolean, encodedFrames: number, now: number): boolean {
    if (!hidden) {
      this.hiddenSince = null;
      this.fired = false;
      return false;
    }
    if (this.hiddenSince === null || encodedFrames !== this.framesAtHidden) {
      // Newly hidden, or frames still flowing while hidden: the silent span
      // starts (over) here.
      this.hiddenSince = now;
      this.framesAtHidden = encodedFrames;
      return false;
    }
    if (this.fired || now - this.hiddenSince < this.thresholdMs) return false;
    this.fired = true;
    return true;
  }

  // Fresh per broadcast: a restart must not inherit the last one's span.
  reset(): void {
    this.hiddenSince = null;
    this.fired = false;
  }
}
