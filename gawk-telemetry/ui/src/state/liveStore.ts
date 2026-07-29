import { create } from 'zustand';

import { fetchLive } from '../api/client.ts';
import type { Snapshot } from '../api/types.ts';

// UD22: the live feed is an EventSource, with the 2 s poll as the fallback.
//
// The projection is unchanged; only its delivery is. What the stream buys is
// that an idle fleet costs a heartbeat instead of a full payload — the server
// hashes the projection and skips an identical one — which is the actual
// pressure at UD12's scale, where every 2 s carries findings and metrics for
// every session on the fleet, times every open view.
//
// The fallback is not a nicety. The stream can fail for reasons that have
// nothing to do with this service (a proxy that buffers, a browser that caps
// concurrent connections, a laptop that slept), and the page that existed
// before R31 already worked without it. So: try the stream, degrade to the
// poll, and SAY which one is feeding the page rather than leaving an operator
// to wonder why nothing is updating.
//
// This file also replaces `lib/history.ts`, which is deleted rather than
// extended (docs/36 §1.1). That layer accumulated a timeline from the polls the
// page already made — 10 minutes, gone on reload, "history starts when the page
// is opened". The full-resolution timeline of a live session was on disk and
// already served the whole time.

export const POLL_MS = 2000;

/**
 * How long after the last successful update the feed is called stale. Three
 * missed cycles: one failure is a blip, three is a fact worth showing.
 */
export const STALE_AFTER_MS = POLL_MS * 3;

export type FeedMode = 'stream' | 'poll' | 'connecting';

interface LiveState {
  snapshot: Snapshot | null;
  error: string | null;
  /** Wall-clock of the last SUCCESSFUL update, for the staleness banner. */
  lastOkAt: number | null;
  mode: FeedMode;
  paused: boolean;
  /** The instant the page was frozen at, so the freeze can name itself. */
  pausedAtMs: number | null;
  /**
   * How long the tab was backgrounded, if it was. TH11's background-throttle
   * honesty: a resumed tab MARKS the gap it did not observe. It never
   * backfills, because the data for that window was never delivered to it.
   */
  gapMs: number | null;

  poll: () => Promise<void>;
  start: () => () => void;
  setPaused: (paused: boolean) => void;
  noteGap: (ms: number) => void;
}

export const useLiveStore = create<LiveState>((set, get) => ({
  snapshot: null,
  error: null,
  lastOkAt: null,
  mode: 'connecting',
  paused: false,
  pausedAtMs: null,
  gapMs: null,

  poll: async () => {
    if (get().paused) return;
    try {
      const snap = await fetchLive();
      set({ snapshot: snap, error: null, lastOkAt: Date.now() });
    } catch (e) {
      // The page KEEPS the last good snapshot and says the feed is stale.
      // Blanking it on one failed poll would be precisely the "absence of
      // evidence rendered as something else" the health model refuses to do.
      set({ error: e instanceof Error ? e.message : String(e) });
    }
  },

  setPaused: (paused) =>
    set((s) => ({
      paused,
      pausedAtMs: paused ? (s.snapshot?.atMs ?? Date.now()) : null,
      // Resuming clears the gap marker: the next update is live again, and a
      // stale "you missed 4 minutes" banner would be its own small lie.
      gapMs: paused ? s.gapMs : null,
    })),

  noteGap: (ms) => set({ gapMs: ms }),

  /**
   * Start the feed. Returns a teardown.
   *
   * The EventSource is attempted first and the poll runs only while the stream
   * is not connected — never both, or an idle fleet would cost exactly what
   * UD22 exists to avoid.
   */
  start: () => {
    let stopped = false;
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setInterval> | null = null;

    const startPolling = () => {
      if (stopped || timer) return;
      set({ mode: 'poll' });
      void get().poll();
      timer = setInterval(() => void get().poll(), POLL_MS);
    };
    const stopPolling = () => {
      if (timer) clearInterval(timer);
      timer = null;
    };

    const connect = () => {
      if (stopped) return;
      if (typeof EventSource === 'undefined') {
        startPolling();
        return;
      }
      try {
        es = new EventSource('live/stream');
      } catch {
        startPolling();
        return;
      }
      es.addEventListener('open', () => {
        // The stream is live; the poll would now be duplicate work.
        stopPolling();
        set({ mode: 'stream', error: null });
      });
      es.addEventListener('snapshot', (ev) => {
        if (get().paused) return;
        try {
          const snap = JSON.parse((ev as MessageEvent).data) as Snapshot;
          set({ snapshot: snap, error: null, lastOkAt: Date.now(), mode: 'stream' });
        } catch {
          // A frame we cannot parse is a frame we ignore; the next one is 2 s
          // away and the last good snapshot is still on screen.
        }
      });
      es.addEventListener('error', () => {
        // EventSource reconnects on its own, but its backoff is opaque and a
        // proxy that refuses the stream outright would leave the page silent
        // forever. Falling back to the poll is what makes the stream optional
        // rather than load-bearing.
        startPolling();
      });
    };

    connect();
    // A safety net for the case the stream never opens AND never errors, which
    // is what a buffering proxy looks like from in here.
    const openCheck = setTimeout(() => {
      if (get().mode === 'connecting') startPolling();
    }, POLL_MS * 2);

    return () => {
      stopped = true;
      clearTimeout(openCheck);
      stopPolling();
      es?.close();
    };
  },
}));

/**
 * Whether the feed has gone quiet. Computed from the last SUCCESS rather than
 * from the error flag: a feed that has been failing for one cycle is not yet
 * worth shouting about, and the last good data stays on screen either way.
 */
export function isStale(lastOkAt: number | null, now = Date.now()): boolean {
  return lastOkAt !== null && now - lastOkAt > STALE_AFTER_MS;
}
