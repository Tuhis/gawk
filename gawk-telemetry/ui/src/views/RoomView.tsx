import { useEffect, useMemo, useState } from 'react';

import { fetchHistorySessions } from '../api/client.ts';
import type { HistoryPage, Severity } from '../api/types.ts';
import { CoverageNote } from '../components/Chrome.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { timeZoneLabel } from '../lib/format.ts';
import { groupByBroadcast } from '../lib/rooms.ts';
import { rank } from '../lib/severity.ts';
import { href, isRoomKey, useDocumentTitle, useUrlState } from '../router/router.ts';
import { Row } from './HistoryView.tsx';
import history from './HistoryView.module.css';
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
    fetchHistorySessions({ fromMs: Date.now() - rangeMs, room: roomKey, limit: 2000 }, ac.signal)
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
      <header className={view.head}>
        <SeverityBadge severity={worst} />
        <h1 className={view.title}>room</h1>
        <span className={view.subtitle}>{roomKey}</span>
        <span className={view.subtitle}>
          {page ? `${groups.length} broadcasts · ${page.total} sessions` : '…'} · times in{' '}
          {timeZoneLabel()}
        </span>
        <span className={view.spacer} />
        <label className={view.control}>
          range
          <select className={view.select} value={range} onChange={(e) => setRange(e.target.value)}>
            {RANGES.map((r) => (
              <option key={r.label} value={r.label}>
                {r.label}
              </option>
            ))}
          </select>
        </label>
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
          {page?.coverage.note
            ? 'See the note above: part of this range is not answerable, which is different from empty.'
            : 'The range is fully covered — nothing reported, rather than nothing kept.'}
        </p>
      ) : (
        groups.map((g) => (
          <section key={g.broadcastKey} className={styles.group} aria-label={`broadcast ${g.broadcastKey}`}>
            <h2 className={styles.groupTitle}>
              <a href={href('broadcast', g.broadcastKey)} className={styles.groupLink}>
                broadcast {g.broadcastKey} →
              </a>
              <span className={styles.groupMeta}>
                {g.broadcaster ? 'broadcaster reporting' : 'no broadcaster session'} · {g.viewers}{' '}
                {g.viewers === 1 ? 'viewer' : 'viewers'}
                {g.live ? ' · live' : ''}
              </span>
            </h2>
            <div className={history.headRow} aria-hidden>
              <span className={history.cSev} />
              <span className={history.cWhen}>started</span>
              <span className={history.cDur}>duration</span>
              <span className={history.cRole}>role</span>
              <span className={history.cId}>session</span>
              <span className={history.cClient}>client</span>
              <span className={history.cNum}>stalls</span>
              <span className={history.cVerdict}>verdict</span>
            </div>
            {g.rows.map((r) => (
              <Row key={r.sessionId} row={r} />
            ))}
          </section>
        ))
      )}
      {loading && <p className={view.loading}>Loading…</p>}
    </div>
  );
}
