// The hidden-tab watchdog. A broadcaster tab left in the background keeps its
// relay session alive — the browser's network process answers the QUIC
// keepalives — while, on the main-thread capture path, its frames stop.
//
// The relay cannot tell that tab from a paused game on a static screen:
// capture is damage-driven, so both deliver no video. The page can: it knows
// it is hidden. So the page ends its own broadcast after BACKGROUND_STOP_MS
// hidden with no frame encoded in that span, and says so on the card when the
// tab comes back.
//
// By design it leaves alone a hidden tab whose frames keep flowing (the
// worker-offload path keeps encoding in the background) and a visible static
// screen. Chrome on macOS also reports a fully occluded window as hidden, and
// the main-thread path stops its frames too, so a fullscreen game covering
// the browser for five minutes ends what was already a frozen stream.

export const BACKGROUND_STOP_MS = 5 * 60 * 1000;

// "Hidden or covered": a fully occluded window counts as hidden (see above).
// Within the broadcast grace a restart reclaims the same code.
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
