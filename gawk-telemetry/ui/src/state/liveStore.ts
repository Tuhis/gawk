import { create } from 'zustand';

import { fetchLive } from '../api/client.ts';
import type { Snapshot } from '../api/types.ts';
import { ingest, type HistoryMap } from '../lib/history.ts';

/** 2 s, matching the projection's own refresh. Polling, not SSE: no connection
 *  state to lose, survives any proxy, and debuggable with curl. */
export const POLL_MS = 2000;

interface LiveState {
  snapshot: Snapshot | null;
  history: HistoryMap;
  /** Last error, kept alongside the last good snapshot rather than replacing it. */
  error: string | null;
  /** Wall-clock of the last SUCCESSFUL poll, for the staleness banner. */
  lastOkAt: number | null;
  poll: () => Promise<void>;
}

export const useLiveStore = create<LiveState>((set, get) => ({
  snapshot: null,
  history: {},
  error: null,
  lastOkAt: null,

  poll: async () => {
    try {
      const snap = await fetchLive();
      const now = Date.now();
      set({
        snapshot: snap,
        history: ingest(get().history, snap, now),
        error: null,
        lastOkAt: now,
      });
    } catch (e) {
      // The page KEEPS the last good snapshot and says the feed is stale.
      // Blanking it on one failed poll would be precisely the "absence of
      // evidence rendered as something else" the health model refuses to do.
      set({ error: e instanceof Error ? e.message : String(e) });
    }
  },
}));
