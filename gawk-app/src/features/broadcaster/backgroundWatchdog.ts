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

export const BACKGROUND_STOP_MS = 5 * 60 * 1000;

export const BACKGROUND_STOP_NOTE =
  'Stopped: the tab was in the background for 5 minutes with no video. Start again when you’re back.';

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
