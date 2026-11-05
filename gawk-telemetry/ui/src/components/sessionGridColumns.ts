import { useMemo } from 'react';

import type { HistoryRow } from '../api/types.ts';
import { useColumns, type ColumnSpec, type Columns } from '../lib/columns.ts';
import { compactTime, dur, EMPTY, shortId } from '../lib/format.ts';

// The history grid's columns (R36, docs/86 TU2), shared by the history
// browser and the room view so both measure the same way.

export function client(r: HistoryRow): string {
  return [r.browser, r.os].filter(Boolean).join(' · ') || EMPTY;
}

export const HISTORY_COLUMNS: ColumnSpec<HistoryRow>[] = [
  { key: 'severity', label: 'severity', samples: ['? UNKNOWN'], extra: 22 },
  { key: 'started', label: 'started', text: (r) => compactTime(r.startedAtMs), samples: ['30 Sept 23:59:59'] },
  { key: 'duration', label: 'duration', text: (r) => dur(r.durationMs) },
  // Broadcaster renders at weight 500; the canvas measures at 400.
  { key: 'role', label: 'role', text: (r) => r.role, samples: ['broadcaster'], extra: 8 },
  {
    key: 'session',
    label: 'session',
    text: (r) => shortId(r.sessionId),
    mono: true,
    // Room for the "rollup" and "live" tags a row may carry.
    extra: 52,
  },
  { key: 'client', label: 'client', text: client, max: 320, squeeze: true, squeezeMin: 140 },
  { key: 'app', label: 'app', text: (r) => r.appVersion ?? '', mono: true, max: 160, squeeze: true, squeezeMin: 64 },
  { key: 'stalls', label: 'stalls', text: (r) => String(r.stalls) },
  { key: 'verdict', label: 'verdict', flex: true, flexMin: 220 },
];

/** Widths for one session grid; `loadKey` names the data load. */
export function useSessionGrid(tableId: string, rows: HistoryRow[], loadKey: string, available = 0): Columns {
  const cols = useMemo(() => HISTORY_COLUMNS, []);
  return useColumns(tableId, cols, rows, loadKey, available);
}
