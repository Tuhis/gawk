import type { HistoryRow } from '../api/types.ts';

// R42 (RM8): the room view's one piece of client-side work. The server filters
// a room's sessions (`/v1/history/sessions?room=`); this groups them by the
// broadcast they were on. Pure, so it is testable without a backend.

export interface BroadcastGroup {
  broadcastKey: string;
  /** Rows in the order the server returned them (newest first by default). */
  rows: HistoryRow[];
  /** The broadcaster's session, when one reported from inside the room. */
  broadcaster?: HistoryRow;
  viewers: number;
  /** Earliest start among the group's rows. */
  firstStartMs: number;
  /** True if any row is still running. */
  live: boolean;
}

/**
 * Group one room's rows by broadcast. Live groups first, then by their newest
 * session, matching the list they were cut from; within a group the server's
 * order is kept.
 */
export function groupByBroadcast(rows: HistoryRow[]): BroadcastGroup[] {
  const byKey = new Map<string, BroadcastGroup>();
  for (const r of rows) {
    let g = byKey.get(r.broadcastKey);
    if (!g) {
      g = { broadcastKey: r.broadcastKey, rows: [], viewers: 0, firstStartMs: 0, live: false };
      byKey.set(r.broadcastKey, g);
    }
    g.rows.push(r);
    if (r.role === 'broadcaster') g.broadcaster ??= r;
    else g.viewers++;
    const at = r.startedAtMs ?? 0;
    if (at && (!g.firstStartMs || at < g.firstStartMs)) g.firstStartMs = at;
    if (r.live) g.live = true;
  }
  const groups = [...byKey.values()];
  const newest = (g: BroadcastGroup) => Math.max(0, ...g.rows.map((r) => r.startedAtMs ?? 0));
  groups.sort((a, b) => Number(b.live) - Number(a.live) || newest(b) - newest(a));
  return groups;
}
