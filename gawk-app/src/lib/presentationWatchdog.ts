// The tier-2 stall watchdog: recovers the iPhone native-fullscreen <video>
// (ManagedMediaSource-fed, docs/27) when it stops on an underrun or a hole.
//
// Why it has to exist: WebKit answers an MSE buffer underrun by PAUSING the
// element and firing `ended` where Chromium stalls and resumes on its own
// (docs/27 finding 1), and a hole in either track's buffered range stops the
// playhead at the hole. Under webkitEnterFullscreen there is no gawk UI, so
// without this the only recovery is the user tapping the system play button.
//
// Modeled on hls.js's gap-controller: on `waiting` / `stalled` / `ended` / a
// starvation `pause`, find the newest buffered range; a playhead behind it
// (in a hole, or on an older range) is seeked into it, a paused element with
// data ahead is play()ed again, and a playing element whose playhead has not
// moved across one retry despite data ahead is nudged forward. Nothing ahead
// yet is a wait, not a recovery — replaying into an empty buffer just stalls
// again — so the watchdog polls until media arrives.
//
// What it must never do (docs/27 "Stall watchdog"):
// - Run outside native fullscreen. The armed element is deliberately paused
//   (Decision 5); `useFullscreen` starts this on webkitbeginfullscreen and
//   stops it before its own exit pause, so only the fullscreen interval is
//   ever guarded.
// - Fight a deliberate pause. The system player has a pause button. A pause
//   is starvation only when the element says so — `ended`, a readyState below
//   HAVE_FUTURE_DATA, or a `waiting`/`stalled` not yet answered by `playing`.
//   Any other pause is the user's, and the watchdog stands down until `play`.
// - Spin. Recoveries are spaced at least RECOVERY_MIN_INTERVAL_MS apart; the
//   wait-for-data poll does not count as one.
//
// Every recovery is counted (surfaced as presentationSurface.stallRecoveries)
// so a capture tells a recovering-but-unhealthy player from a healthy one.

// Minimum spacing between two recovery actions (seek and/or play).
export const RECOVERY_MIN_INTERVAL_MS = 1000;
// Re-check cadence while a stall is unresolved (waiting for data, or
// verifying that a recovery took).
export const RECOVERY_RETRY_MS = 250;
// How much media must sit ahead of the playhead before re-playing is worth it.
export const RECOVERY_MIN_AHEAD_S = 0.1;
// How far a stuck-but-fed playhead is nudged (hls.js's nudge, same order).
export const RECOVERY_NUDGE_S = 0.1;
// After an underrun at the live edge, a paused element waits until the rejoin
// offset (the delivery mode's cushion, liveEdge.ts) lies ahead before play():
// appends arrive at 1x, so what is ahead at the resume is the cushion until the
// next seek. If the buffer stops growing (MMS may stop asking for data while
// the element is paused), it resumes with what it has once the stall has
// lasted the rejoin offset plus this grace.
export const CUSHION_GRACE_MS = 1000;

// HTMLMediaElement.HAVE_FUTURE_DATA: below it, a pause is the element starving.
const HAVE_FUTURE_DATA = 3;

export type StallCause = 'waiting' | 'stalled' | 'ended' | 'pause';

// The slice of HTMLMediaElement the watchdog touches (a seam for the tests).
export interface WatchdogMedia extends EventTarget {
  currentTime: number;
  readonly paused: boolean;
  readonly ended: boolean;
  readonly seeking: boolean;
  readonly readyState: number;
  readonly buffered: { readonly length: number; start(i: number): number; end(i: number): number };
  play(): Promise<void> | void;
}

export interface PresentationWatchdogStats {
  // Recovery actions taken (each a seek, a play(), or both).
  recoveries: number;
  // How many of those moved the playhead (hole skip, live-edge landing, nudge).
  seeks: number;
  // play() rejections during a recovery (e.g. NotAllowedError).
  playErrors: number;
  // The event that triggered the most recent recovery.
  lastCause: StallCause | null;
}

export interface PresentationWatchdogOptions {
  // Where a seek into the newest range lands: this far inside its end (but
  // never before its start) — the same rejoin offset the entry seek uses.
  rejoinS: number;
  now?: () => number;
}

const EVENTS = ['waiting', 'stalled', 'ended', 'pause', 'play', 'playing'] as const;

export class PresentationWatchdog {
  private media: WatchdogMedia | null = null;
  private active = false;
  private userPaused = false;
  private stalled = false;
  // When this stall's cushion wait began: the first check that found the
  // element paused or ended, not the first stall event (WebKit can say
  // `waiting` long before it pauses). Null until then; reset per stall.
  private cushionWaitFrom: number | null = null;
  private cause: StallCause | null = null;
  // The playhead when a fed-but-stuck element was first seen; null until then.
  private stuckAt: number | null = null;
  private lastRecoveryAt = Number.NEGATIVE_INFINITY;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private readonly stats: PresentationWatchdogStats = {
    recoveries: 0,
    seeks: 0,
    playErrors: 0,
    lastCause: null,
  };
  private rejoinS: number;
  private readonly now: () => number;

  constructor(opts: PresentationWatchdogOptions) {
    this.rejoinS = opts.rejoinS;
    this.now = opts.now ?? (() => performance.now());
  }

  // The viewer can switch delivery mode mid-session, which moves the live-edge
  // cushion (liveEdge.ts); later recovery seeks land at the new one.
  setRejoinS(rejoinS: number): void {
    this.rejoinS = rejoinS;
  }

  // Binds an element (listeners only; guarding starts with start()). Counters
  // persist across re-attaches: they describe the session, not the element.
  attach(media: WatchdogMedia): void {
    if (this.media === media) return;
    this.detach();
    this.media = media;
    for (const t of EVENTS) media.addEventListener(t, this.onEvent);
  }

  detach(): void {
    this.stop();
    if (!this.media) return;
    for (const t of EVENTS) this.media.removeEventListener(t, this.onEvent);
    this.media = null;
  }

  // Native fullscreen began. Idempotent: a second start() (the toggle and the
  // webkitbeginfullscreen event both call it) must not forget a user pause.
  start(): void {
    if (this.active) return;
    this.active = true;
    this.userPaused = false;
    this.clearStall();
  }

  // Native fullscreen ended — called BEFORE the exit path's own pause(), so
  // that pause is never mistaken for anything.
  stop(): void {
    this.active = false;
    this.clearStall();
  }

  getStats(): PresentationWatchdogStats {
    return { ...this.stats };
  }

  private readonly onEvent = (e: Event) => {
    const media = this.media;
    if (!this.active || !media) return;
    switch (e.type) {
      case 'play':
        this.userPaused = false;
        return;
      case 'playing':
        this.clearStall();
        return;
      case 'ended':
        // A live presentation never legitimately ends; there is no user
        // gesture that produces this.
        this.userPaused = false;
        this.markStalled('ended');
        break;
      case 'pause':
        if (this.stalled || media.ended || media.readyState < HAVE_FUTURE_DATA) {
          this.markStalled('pause');
        } else {
          // Paused with media in hand: the system player's pause button.
          this.userPaused = true;
          this.clearStall();
          return;
        }
        break;
      case 'waiting':
      case 'stalled':
        if (this.userPaused) return;
        this.markStalled(e.type);
        break;
    }
    this.check();
  };

  private markStalled(cause: StallCause): void {
    if (!this.stalled) {
      this.stalled = true;
      this.stuckAt = null;
      this.cushionWaitFrom = null;
    }
    this.cause = cause;
  }

  private clearStall(): void {
    this.stalled = false;
    this.stuckAt = null;
    this.cushionWaitFrom = null;
    this.cancelTimer();
  }

  private cancelTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  private schedule(ms: number): void {
    this.cancelTimer();
    this.timer = setTimeout(() => {
      this.timer = null;
      this.check();
    }, ms);
  }

  private check(): void {
    this.cancelTimer();
    const media = this.media;
    if (!media || !this.active || this.userPaused || !this.stalled) return;
    if (media.seeking) return this.schedule(RECOVERY_RETRY_MS);

    let newestStart: number;
    let newestEnd: number;
    let t: number;
    try {
      const b = media.buffered;
      if (b.length === 0) return this.schedule(RECOVERY_RETRY_MS);
      newestStart = b.start(b.length - 1);
      newestEnd = b.end(b.length - 1);
      t = media.currentTime;
    } catch {
      return this.schedule(RECOVERY_RETRY_MS);
    }

    let seekTo: number | null = null;
    if (t < newestStart) {
      // In a hole or on an older range: the newest range is the only one that
      // grows, so that is where playback has to be.
      seekTo = this.landing(newestStart, newestEnd);
    } else if (newestEnd - t < RECOVERY_MIN_AHEAD_S) {
      // At the live edge with nothing ahead yet: wait for appends.
      return this.schedule(RECOVERY_RETRY_MS);
    } else if (media.ended || media.paused) {
      // An underrun at the live edge. Rebuild the cushion by waiting rather
      // than by seeking back over frames already shown (bounded, see
      // CUSHION_GRACE_MS).
      if (this.cushionWaitFrom === null) this.cushionWaitFrom = this.now();
      const waited = this.now() - this.cushionWaitFrom;
      if (newestEnd - t < this.rejoinS && waited < this.rejoinS * 1000 + CUSHION_GRACE_MS) {
        return this.schedule(RECOVERY_RETRY_MS);
      }
      // play() on an ended element restarts from the beginning; seek first —
      // in place, or forward to the cushion if more than that arrived.
      if (media.ended) seekTo = Math.max(t, this.landing(newestStart, newestEnd));
      // else: paused with data ahead — play() alone.
    } else {
      // Playing, fed, and `waiting`: give the decoder one retry to move
      // before calling it stuck.
      if (this.stuckAt === null || t > this.stuckAt + 0.01) {
        this.stuckAt = t;
        return this.schedule(RECOVERY_RETRY_MS);
      }
      seekTo = Math.min(t + RECOVERY_NUDGE_S, newestEnd - RECOVERY_MIN_AHEAD_S / 2);
    }

    const wait = this.lastRecoveryAt + RECOVERY_MIN_INTERVAL_MS - this.now();
    if (wait > 0) return this.schedule(wait);

    this.lastRecoveryAt = this.now();
    this.stats.recoveries++;
    this.stats.lastCause = this.cause;
    this.stuckAt = null;
    try {
      if (seekTo !== null) {
        media.currentTime = seekTo;
        this.stats.seeks++;
      }
      if (media.paused) {
        const p = media.play();
        if (p && typeof p.catch === 'function') {
          p.catch(() => {
            this.stats.playErrors++;
          });
        }
      }
    } catch {
      // seek/play quirks: counted as a recovery attempt, verified on the retry
    }
    // Verify on the next tick; `playing` cancels this when the recovery took.
    this.schedule(RECOVERY_RETRY_MS);
  }

  private landing(start: number, end: number): number {
    return Math.max(start, end - this.rejoinS);
  }
}
