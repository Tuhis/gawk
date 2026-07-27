import { create } from 'zustand';

import type { Severity } from '../api/types.ts';

// View state that must survive a data refresh. The old vanilla page rebuilt its
// DOM every 2 s and lost all of this, which made a card unreadable — a human
// reading a session table is slower than the poll. React reconciles instead of
// rebuilding, so the state simply lives here.

interface UiState {
  /**
   * The operator's DISAGREEMENT with the severity default, per broadcast — not
   * raw open-state. A card opens by itself when something is wrong, and
   * remembering raw open-state would pin it open long after the fault cleared,
   * defeating the auto-open the page is built around. Once the default agrees
   * with the choice the entry is dropped and severity speaks again.
   */
  cardOverrides: Record<string, boolean>;
  /** Expanded timeline panels, keyed by sessionId. Purely the operator's call. */
  openTimelines: Record<string, boolean>;
  /** Obfuscated key the find box matched, highlighted until cleared. */
  foundKey: string | null;

  setCardOpen: (key: string, open: boolean, byDefault: boolean) => void;
  isCardOpen: (key: string, byDefault: boolean) => boolean;
  toggleTimeline: (sessionId: string) => void;
  setFoundKey: (key: string | null) => void;
  /** Drop entries for broadcasts that are no longer on the page. */
  prune: (liveKeys: Set<string>) => void;
}

export const useUiStore = create<UiState>((set, get) => ({
  cardOverrides: {},
  openTimelines: {},
  foundKey: null,

  setCardOpen: (key, open, byDefault) =>
    set((s) => {
      const next = { ...s.cardOverrides };
      if (open === byDefault) delete next[key];
      else next[key] = open;
      return { cardOverrides: next };
    }),

  isCardOpen: (key, byDefault) => {
    const o = get().cardOverrides[key];
    return o === undefined ? byDefault : o;
  },

  toggleTimeline: (sessionId) =>
    set((s) => ({ openTimelines: { ...s.openTimelines, [sessionId]: !s.openTimelines[sessionId] } })),

  setFoundKey: (foundKey) => set({ foundKey }),

  prune: (liveKeys) =>
    set((s) => {
      const next: Record<string, boolean> = {};
      for (const [k, v] of Object.entries(s.cardOverrides)) if (liveKeys.has(k)) next[k] = v;
      return Object.keys(next).length === Object.keys(s.cardOverrides).length
        ? s
        : { cardOverrides: next };
    }),
}));

/** Whether a card opens on its own: something is wrong and worth looking at. */
export function opensByDefault(severity: Severity): boolean {
  return severity === 'bad' || severity === 'warn';
}
