import type { Snapshot } from '../api/types.ts';

// Per-session time series for the timeline graphs.
//
// The history is accumulated CLIENT-SIDE from the polls the page already makes,
// not fetched: `/live` is a point-in-time projection and carries no series, and
// the stored per-sample timeline on disk only exists once a session has been
// finalised — which is exactly when it stops being live. So the page remembers
// what it has seen.
//
// The honest consequence, and it must be surfaced rather than hidden: history
// starts when the page is opened. A graph covering less than its window says so
// instead of drawing a short line as though that were the whole story — the
// same rule the severity model follows, where an absence of evidence is never
// dressed up as evidence.

/** 10 minutes. At the 2 s poll cadence that is ~300 points per session. */
export const WINDOW_MS = 10 * 60 * 1000;

/**
 * Hard cap per session, independent of the window. The window is enforced by
 * timestamp, but a clock jump or a burst of manual refreshes must not be able
 * to grow a buffer without bound.
 */
export const MAX_POINTS = 600;

/**
 * How long a vanished session's history is kept. A broadcast that ends moves to
 * the recessed group and its rows keep their graphs for a while — losing the
 * picture at the exact moment something finished would be perverse.
 */
export const RETAIN_AFTER_GONE_MS = 5 * 60 * 1000;

export interface Point {
  t: number;
  /** Only the numeric metrics; absent keys stay absent (see api/types). */
  v: Record<string, number>;
}

export interface SessionHistory {
  points: Point[];
  /** Wall-clock of the last snapshot this session appeared in. */
  lastSeen: number;
}

export type HistoryMap = Record<string, SessionHistory>;

/**
 * Fold one snapshot into the history map, returning a NEW map (the store keeps
 * it immutable so React re-renders predictably).
 *
 * `now` is passed rather than read so the whole thing is testable without
 * faking timers.
 */
export function ingest(prev: HistoryMap, snap: Snapshot, now: number): HistoryMap {
  const next: HistoryMap = { ...prev };
  const cutoff = now - WINDOW_MS;

  for (const group of [snap.live ?? [], snap.ended ?? []]) {
    for (const b of group) {
      for (const s of b.sessions ?? []) {
        const existing = next[s.sessionId];
        const points = existing ? existing.points : [];
        // `atMs` is the projection's own clock, which keeps the series aligned
        // with the data rather than with when the browser happened to render.
        const point: Point = { t: snap.atMs, v: { ...(s.metrics ?? {}) } };
        // A repeated snapshot (a manual refresh, or a poll that raced) must not
        // stack duplicate points on one timestamp.
        const appended =
          points.length && points[points.length - 1].t === point.t
            ? [...points.slice(0, -1), point]
            : [...points, point];
        const trimmed = appended.filter((p) => p.t >= cutoff).slice(-MAX_POINTS);
        next[s.sessionId] = { points: trimmed, lastSeen: now };
      }
    }
  }

  // Drop sessions that have been gone long enough to be uninteresting, so the
  // map cannot grow for the lifetime of the tab.
  for (const id of Object.keys(next)) {
    if (now - next[id].lastSeen > RETAIN_AFTER_GONE_MS) delete next[id];
  }
  return next;
}

/** Extract one metric as a series, dropping points where it was absent. */
export function series(history: SessionHistory | undefined, key: string): Point[] {
  if (!history) return [];
  return history.points.filter((p) => Number.isFinite(p.v[key]));
}

/**
 * How much wall-clock the buffer actually covers. The graphs use this to say
 * "collecting…" rather than implying a full window they do not have.
 */
export function coverageMs(history: SessionHistory | undefined): number {
  if (!history || history.points.length < 2) return 0;
  return history.points[history.points.length - 1].t - history.points[0].t;
}
