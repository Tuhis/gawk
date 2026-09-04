import { useCallback, useEffect, useMemo, useState } from 'react';

import { fetchHistorySessions, type HistoryParams } from '../api/client.ts';
import type { HistoryPage, HistoryRow } from '../api/types.ts';
import { CoverageNote } from '../components/Chrome.tsx';
import { FindStream } from '../components/FindStream.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { VirtualRows } from '../components/VirtualRows.tsx';
import { absoluteTime, dur, EMPTY, shortId, timeZoneLabel } from '../lib/format.ts';
import { href, useDocumentTitle, useUrlState } from '../router/router.ts';
import { useUiStore } from '../state/uiStore.ts';
import styles from './HistoryView.module.css';
import view from './view.module.css';

// TH3: the history browser. Q2 — *"it was bad at 21:04, show me"* — is
// unanswerable in a browser today, because nothing before the tab opened
// exists. This is where it becomes answerable.
//
// **Every filter, sort and page is server-side** (UD4). Shipping 30 days of
// rollups to a browser to filter them there is the same category error as
// returning 80 fields to a model, and the request shape is what a test asserts.
//
// The columns are `SessionSummary`'s projection, which R28 built for exactly
// this and nothing consumed.

const RANGES: Array<{ label: string; ms: number }> = [
  { label: '1h', ms: 3600_000 },
  { label: '6h', ms: 6 * 3600_000 },
  { label: '24h', ms: 24 * 3600_000 },
  { label: '7d', ms: 7 * 24 * 3600_000 },
  { label: '30d', ms: 30 * 24 * 3600_000 },
];

const ROW_HEIGHT = 26;

export function HistoryView() {
  useDocumentTitle('history — gawk telemetry');
  // Every one of these lives in the URL, not in a store the address bar knows
  // nothing about: "send me the link" is a real operator move (Q10).
  const [range, setRange] = useUrlState('range', '24h');
  const [role, setRole] = useUrlState('role');
  const [verdict, setVerdict] = useUrlState('verdict');
  const [sort, setSort] = useUrlState('sort', 'start');
  const [broadcast, setBroadcast] = useUrlState('broadcast');
  const [appVersion, setAppVersion] = useUrlState('appVersion');
  const [findingsOnly, setFindingsOnly] = useUrlState('hasFindings');
  const [distrusted, setDistrusted] = useUrlState('distrusted');

  const [page, setPage] = useState<HistoryPage | null>(null);
  const [rows, setRows] = useState<HistoryRow[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const foundKey = useUiStore((s) => s.foundKey);

  const rangeMs = RANGES.find((r) => r.label === range)?.ms ?? 24 * 3600_000;
  // The Find box scopes the browser rather than merely highlighting a live row
  // — which is what it did before, and is the smaller half of what it is for.
  const effectiveBroadcast = broadcast || foundKey || '';

  const params = useMemo<HistoryParams>(
    () => ({
      fromMs: Date.now() - rangeMs,
      role: role || undefined,
      verdict: verdict || undefined,
      broadcast: effectiveBroadcast || undefined,
      appVersion: appVersion || undefined,
      hasFindings: findingsOnly === '1' ? true : undefined,
      distrusted: distrusted === '1' ? true : undefined,
      sort,
      limit: 200,
    }),
    [rangeMs, role, verdict, effectiveBroadcast, appVersion, findingsOnly, distrusted, sort],
  );

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    fetchHistorySessions(params, ac.signal)
      .then((p) => {
        setPage(p);
        setRows(p.rows);
        setError(null);
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => setLoading(false));
    return () => ac.abort();
  }, [params]);

  // Cursor paging, driven by the virtualizer reaching the end. The browser
  // holds one page at a time plus what it has scrolled through, never the
  // unfiltered set.
  const loadMore = useCallback(() => {
    const cursor = page?.nextCursor;
    if (!cursor || loading) return;
    setLoading(true);
    fetchHistorySessions({ ...params, cursor })
      .then((p) => {
        setPage(p);
        setRows((prev) => [...prev, ...p.rows]);
      })
      .catch(() => undefined)
      .finally(() => setLoading(false));
  }, [page?.nextCursor, params, loading]);

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>History</h1>
        <span className={view.subtitle}>
          {page ? `${rows.length} of ${page.total}` : '…'} · times in {timeZoneLabel()}
        </span>
        <span className={view.spacer} />
        <FindStream />
      </header>

      <div className={view.controls}>
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
        <label className={view.control}>
          role
          <select className={view.select} value={role} onChange={(e) => setRole(e.target.value)}>
            <option value="">any</option>
            <option value="viewer">viewer</option>
            <option value="broadcaster">broadcaster</option>
          </select>
        </label>
        <label className={view.control}>
          verdict
          <select className={view.select} value={verdict} onChange={(e) => setVerdict(e.target.value)}>
            <option value="">any</option>
            <option value="bad">bad</option>
            <option value="warn">warn</option>
            <option value="ok">ok</option>
            <option value="unknown">unknown</option>
          </select>
        </label>
        <label className={view.control}>
          sort
          <select className={view.select} value={sort} onChange={(e) => setSort(e.target.value)}>
            <option value="start">newest</option>
            <option value="severity">worst first</option>
            <option value="duration">longest</option>
            <option value="stalls">most stalls</option>
          </select>
        </label>
        <label className={view.control}>
          version
          <input
            className={view.text}
            value={appVersion}
            placeholder="any"
            onChange={(e) => setAppVersion(e.target.value)}
          />
        </label>
        <button
          type="button"
          className={`${view.button} ${findingsOnly === '1' ? view.buttonOn : ''}`}
          onClick={() => setFindingsOnly(findingsOnly === '1' ? '' : '1')}
        >
          has findings
        </button>
        <button
          type="button"
          className={`${view.button} ${distrusted === '1' ? view.buttonOn : ''}`}
          onClick={() => setDistrusted(distrusted === '1' ? '' : '1')}
        >
          distrusted
        </button>
        {effectiveBroadcast && (
          <button type="button" className={view.button} onClick={() => setBroadcast('')}>
            broadcast {shortId(effectiveBroadcast)} ×
          </button>
        )}
      </div>

      <CoverageNote coverage={page?.coverage} />
      {error && <p className={view.error}>{error}</p>}

      <div className={styles.headRow} aria-hidden>
        <span className={styles.cSev} />
        <span className={styles.cWhen}>started</span>
        <span className={styles.cDur}>duration</span>
        <span className={styles.cRole}>role</span>
        <span className={styles.cId}>session</span>
        <span className={styles.cClient}>client</span>
        <span className={styles.cNum}>stalls</span>
        <span className={styles.cVerdict}>verdict</span>
      </div>

      {rows.length === 0 && !loading ? (
        <p className={view.rollupOnly}>
          No sessions in this range.{' '}
          {page?.coverage.note
            ? 'See the note above: part of this range is not answerable, which is different from empty.'
            : 'The range is fully covered — this really is nothing rather than nothing kept.'}
        </p>
      ) : (
        <VirtualRows
          rows={rows}
          rowHeight={ROW_HEIGHT}
          height={Math.min(640, Math.max(200, rows.length * ROW_HEIGHT + 8))}
          keyOf={(r) => r.sessionId}
          ariaLabel="sessions"
          onEndReached={loadMore}
          renderRow={(r) => <Row row={r} />}
        />
      )}
      {loading && <p className={view.loading}>Loading…</p>}
    </div>
  );
}

/**
 * One history row as a link to its session. Exported for the room view (R42),
 * which lists the same rows grouped by broadcast — one row shape, one renderer.
 */
export function Row({ row }: { row: HistoryRow }) {
  return (
    <a className={styles.row} href={href('session', row.sessionId)}>
      <span className={styles.cSev}>
        <SeverityBadge severity={row.severity} />
      </span>
      <span className={`${styles.cWhen} tnum`}>{absoluteTime(row.startedAtMs)}</span>
      <span className={`${styles.cDur} tnum`}>{dur(row.durationMs)}</span>
      <span className={styles.cRole}>{row.role}</span>
      <span className={`${styles.cId} ${styles.mono}`}>
        {shortId(row.sessionId)}
        {/* UD10's boundary, drawn on the row itself: this one has a permanent
            verdict and no samples left behind it. */}
        {row.rollupOnly && <span className={styles.tag} title="raw samples pruned — rollup only">R</span>}
        {row.live && <span className={styles.tagLive} title="still running">●</span>}
      </span>
      <span className={styles.cClient}>
        {[row.browser, row.os].filter(Boolean).join(' / ') || EMPTY}
        {row.appVersion ? ` · ${row.appVersion}` : ''}
      </span>
      <span className={`${styles.cNum} tnum`}>{row.stalls}</span>
      <span className={styles.cVerdict} title={row.distrust || row.verdict}>
        {row.distrust ? `⚠ ${row.distrust}` : row.verdict || EMPTY}
      </span>
    </a>
  );
}
