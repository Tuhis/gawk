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
  /**
   * TH11/UD19's watch list: starred broadcasts pin to the top and visibly
   * change on escalation.
   *
   * **In-tab only, and nothing more.** No browser notifications — Chrome
   * throttles a background tab to ~1/min, so an alert could arrive a minute
   * late, which for a stuttering stream is worse than useless — no sound, and
   * no server state. R28's non-goal was alerting INFRASTRUCTURE; a page
   * noticing something while you look at it is not that.
   */
  watched: Record<string, true>;
  /** Severity a watched broadcast was last seen at, so an escalation is visible. */
  watchSeverity: Record<string, Severity>;

  setCardOpen: (key: string, open: boolean, byDefault: boolean) => void;
  isCardOpen: (key: string, byDefault: boolean) => boolean;
  toggleTimeline: (sessionId: string) => void;
  setFoundKey: (key: string | null) => void;
  toggleWatch: (key: string) => void;
  observeSeverity: (key: string, severity: Severity) => void;
  /** Drop entries for broadcasts that are no longer on the page. */
  prune: (liveKeys: Set<string>) => void;
}

export const useUiStore = create<UiState>((set, get) => ({
  cardOverrides: {},
  openTimelines: {},
  foundKey: null,
  watched: {},
  watchSeverity: {},

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

  toggleWatch: (key) =>
    set((s) => {
      const watched = { ...s.watched };
      const watchSeverity = { ...s.watchSeverity };
      if (watched[key]) {
        delete watched[key];
        delete watchSeverity[key];
      } else {
        watched[key] = true;
      }
      return { watched, watchSeverity };
    }),

  // The baseline a watched broadcast is judged against. Recorded on the first
  // observation after starring, and then only LOWERED when severity improves —
  // so an escalation stays visible until the operator acknowledges it by
  // unstarring, rather than disappearing the moment the fault clears. That
  // asymmetry is the same one the live projection already uses: problems should
  // appear promptly and must not vanish before a human finishes looking.
  observeSeverity: (key, severity) =>
    set((s) => {
      if (!s.watched[key]) return s;
      const prev = s.watchSeverity[key];
      if (prev === undefined) return { watchSeverity: { ...s.watchSeverity, [key]: severity } };
      return s;
    }),

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

const RANK: Record<Severity, number> = { bad: 3, warn: 2, unknown: 1, ok: 0 };

/**
 * Whether a watched broadcast has got WORSE since it was starred. This is the
 * whole of UD19's alerting: a row that visibly changes while you are looking at
 * the page.
 */
export function hasEscalated(from: Severity | undefined, now: Severity): boolean {
  if (from === undefined) return false;
  return RANK[now] > RANK[from];
}
