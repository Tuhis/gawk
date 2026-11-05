import { useEffect, useMemo, useState } from 'react';

import { fetchHistorySessions } from '../api/client.ts';
import type { HistoryPage, Severity } from '../api/types.ts';
import { CoverageNote, DataSince } from '../components/Chrome.tsx';
import { DragLine, WidthsNote } from '../components/Columns.tsx';
import { GridHead, GridRow } from '../components/SessionGrid.tsx';
import { HISTORY_COLUMNS, useSessionGrid } from '../components/sessionGridColumns.ts';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { groupByBroadcast } from '../lib/rooms.ts';
import { rank } from '../lib/severity.ts';
import { href, isRoomKey, useDocumentTitle, useUrlState } from '../router/router.ts';
import t from '../components/tables.module.css';
import styles from './RoomView.module.css';
import view from './view.module.css';

// R42 (RM8, docs/44 §4.10): a room's sessions, grouped by broadcast.
//
// A room holds several broadcasts and every viewer that watched from inside
// it. The question this page answers is "what happened in this room" rather
// than "what happened on this stream" — so it is one history query scoped by
// the room key (`/v1/history/sessions?room=`), grouped here by broadcast. The
// server already filters, sorts and pages (UD4); the grouping is the only work
// the browser does, and it is a pure function so it can be tested without a
// backend.
//
// The key is the relay's HMAC of the room code, the same shape and posture as a
// broadcast key: the page never sees a room CODE, and a Find box resolving one
// goes through `resolveRoom` server-side.

const RANGES = [
  { label: '24h', ms: 24 * 3600_000 },
  { label: '7d', ms: 7 * 24 * 3600_000 },
  { label: '14d', ms: 14 * 24 * 3600_000 },
  { label: '30d', ms: 30 * 24 * 3600_000 },
];

interface Props {
  roomKey: string;
}

export function RoomView({ roomKey }: Props) {
  useDocumentTitle(`room ${roomKey.slice(0, 8)} — gawk telemetry`);
  const [range, setRange] = useUrlState('range', '7d');
  const [page, setPage] = useState<HistoryPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // The instant the loaded range starts, kept with the load it belongs to.
  const [fromMs, setFromMs] = useState(0);

  const rangeMs = RANGES.find((r) => r.label === range)?.ms ?? 7 * 24 * 3600_000;
  const valid = isRoomKey(roomKey);

  useEffect(() => {
    if (!valid) {
      setLoading(false);
      return;
    }
    const ac = new AbortController();
    setLoading(true);
    // One room is small (a handful of broadcasts, tens of viewers), so the
    // page asks for the history browser's ceiling rather than paging: a
    // grouped view that stopped mid-broadcast would misreport that broadcast.
    const from = Date.now() - rangeMs;
    setFromMs(from);
    fetchHistorySessions({ fromMs: from, room: roomKey, limit: 2000 }, ac.signal)
      .then((p) => {
        setPage(p);
        setError(null);
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => setLoading(false));
    return () => ac.abort();
  }, [roomKey, rangeMs, valid]);

  const groups = useMemo(() => groupByBroadcast(page?.rows ?? []), [page]);
  // One set of widths for every group on the page, measured over all of
  // them, so the groups' columns line up with each other.
  const allRows = useMemo(() => page?.rows ?? [], [page]);
  const columns = useSessionGrid('room', allRows, `${roomKey}:${rangeMs}`);

  if (!valid) {
    return (
      <div className={view.page}>
        <p className={view.error}>“{roomKey}” is not a room key (12 hex characters).</p>
        <p className={view.note}>
          <a href={href('history')}>Browse history →</a>
        </p>
      </div>
    );
  }

  const worst = (page?.rows ?? []).reduce<Severity>(
    (acc, r) => (rank(r.severity) > rank(acc) ? r.severity : acc),
    'unknown',
  );

  return (
    <div className={view.page}>
      <div className={view.crumbs}>
        <a href={href('history')}>History</a>
        <span>/</span>
        <span>room</span>
      </div>
      <header className={view.head}>
        <SeverityBadge severity={worst} />
        <h1 className={view.title}>
          Room <span className={view.titleId}>{roomKey}</span>
        </h1>
        <span className={view.subtitle}>
          {page ? `${groups.length} broadcast${groups.length === 1 ? '' : 's'} · ${page.total} sessions` : '…'}
        </span>
        <span className={view.spacer} />
        <DataSince coverage={page?.coverage} fromMs={fromMs} />
        <div className={view.seg} role="group" aria-label="Range">
          {RANGES.map((r) => (
            <button key={r.label} type="button" aria-pressed={range === r.label} onClick={() => setRange(r.label)}>
              {r.label}
            </button>
          ))}
        </div>
      </header>

      <p className={view.note}>
        Every session that reported from inside this room, grouped by the broadcast it was on. The
        room key is the relay’s HMAC of the room code — a session’s own statement of where it was,
        not a relay-verified fact.
      </p>

      {page?.coverage && <CoverageNote coverage={page.coverage} />}
      {error && <p className={view.error}>{error}</p>}

      {!loading && !error && groups.length === 0 ? (
        <p className={view.rollupOnly}>
          No sessions reported from this room in the last {range}.{' '}
          {page?.coverage.reason === 'none'
            ? 'This service has nothing stored at all yet — the range is unanswerable, not empty.'
            : page?.coverage.reason === 'before-data'
              ? 'Nothing is missing: the service has only been collecting since the date beside the range.'
              : 'The range is fully covered — nothing reported, rather than nothing kept.'}
        </p>
      ) : (
        groups.map((g) => (
          <section key={g.broadcastKey} className={`${view.panel} ${view.panelClip}`} aria-label={`broadcast ${g.broadcastKey}`}>
            <div className={view.panelH}>
              <h2 className={view.panelT}>
                <a href={href('broadcast', g.broadcastKey)} className={styles.groupLink}>
                  Broadcast {g.broadcastKey} →
                </a>
              </h2>
              <span className={view.note}>
                {g.broadcaster ? 'broadcaster reporting' : 'no broadcaster session'} · {g.viewers}{' '}
                {g.viewers === 1 ? 'viewer' : 'viewers'}
                {g.live ? ' · live' : ''}
              </span>
            </div>
            <div className={t.wrap}>
              <div style={{ minWidth: columns.minTotal }} role="table" aria-label={`sessions of broadcast ${g.broadcastKey}`}>
                <GridHead columns={columns} />
                {g.rows.map((r) => (
                  <GridRow key={r.sessionId} row={r} columns={columns} />
                ))}
              </div>
              <DragLine columns={columns} cols={HISTORY_COLUMNS} />
            </div>
          </section>
        ))
      )}
      {groups.length > 0 && (
        <p className={`${t.foot} ${styles.widths}`}>
          <span className={view.spacer} />
          <WidthsNote columns={columns} cols={HISTORY_COLUMNS} />
        </p>
      )}
      {loading && <p className={view.loading}>Loading…</p>}
    </div>
  );
}
