// Was this tab in the background, and for how long?
//
// It matters because a hidden tab stops being a fair witness to itself. The
// browser stops firing `requestAnimationFrame`, so a viewer's `renderedFps`
// falls to 0 while decode carries on perfectly — and nothing downstream could
// tell that apart from a rendering failure, because the difference is not in
// the numbers. It is in a fact only the document knows.
//
// Two fields, and the pair is deliberate:
//
//   * `documentHidden` — the state at the instant the sample was taken.
//   * `documentHiddenMs` — a CUMULATIVE counter of time spent hidden.
//
// The counter is what makes an interval answerable. A sample is an instant, but
// every rate in these stats is measured over the gap between samples, so
// "hidden right now" cannot say whether the window that produced a number was
// clean. A delta between two counter readings can, and counter-deltas are
// already how the live projection and the rollup read this kind of signal
// (docs/33 TM10) — so this composes with machinery that exists rather than
// needing its own.
//
// Lives on the main thread by necessity: `document` does not exist in a worker.
// That keeps D13 intact — collection stays main-thread and adds no worker
// message, on either surface.

export interface VisibilitySample {
  documentHidden: boolean;
  documentHiddenMs: number;
}

// Module state, following the same pattern as playout.ts and av-sync.ts: one
// tracker per page, started lazily, never torn down. A viewer and a broadcaster
// in one document share it, which is correct — they share the document whose
// visibility this is.
let started = false;
let hiddenTotalMs = 0;
let hiddenSince: number | null = null;

function nowMs(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

function onVisibilityChange(): void {
  if (document.visibilityState === 'hidden') {
    // Guard against a duplicate 'hidden' (some browsers fire around bfcache):
    // starting a second interval would double-count the first.
    if (hiddenSince === null) hiddenSince = nowMs();
    return;
  }
  if (hiddenSince !== null) {
    hiddenTotalMs += nowMs() - hiddenSince;
    hiddenSince = null;
  }
}

function ensureStarted(): void {
  if (started || typeof document === 'undefined') return;
  started = true;
  // If the page was ALREADY hidden when collection began — a stream started in
  // a background tab, or restored from bfcache — the interval starts now rather
  // than being missed entirely.
  if (document.visibilityState === 'hidden') hiddenSince = nowMs();
  document.addEventListener('visibilitychange', onVisibilityChange);
}

/**
 * Read the current visibility state. Safe to call on every stats tick.
 *
 * The counter includes the in-progress interval, so a tab that has been hidden
 * for a minute reports ~60000 without waiting to become visible again. A
 * counter that only advanced on the way back would read 0 for exactly as long
 * as the condition it describes was true.
 */
export function readVisibility(): VisibilitySample {
  ensureStarted();
  if (typeof document === 'undefined') {
    return { documentHidden: false, documentHiddenMs: 0 };
  }
  const open = hiddenSince === null ? 0 : nowMs() - hiddenSince;
  return {
    documentHidden: document.visibilityState === 'hidden',
    documentHiddenMs: Math.round(hiddenTotalMs + open),
  };
}

/** Test seam: reset the module tracker. Never called by production code. */
export function __resetVisibilityForTests(): void {
  if (started && typeof document !== 'undefined') {
    document.removeEventListener('visibilitychange', onVisibilityChange);
  }
  started = false;
  hiddenTotalMs = 0;
  hiddenSince = null;
}
