import { useCallback, useEffect, useMemo, useState } from 'react';

import { fetchHistorySessions, type HistoryParams } from '../api/client.ts';
import type { HistoryPage, HistoryRow, Severity } from '../api/types.ts';
import { CoverageNote, DataSince } from '../components/Chrome.tsx';
import { DragLine, WidthsNote } from '../components/Columns.tsx';
import { FindStream } from '../components/FindStream.tsx';
import { GridHead, GridRow } from '../components/SessionGrid.tsx';
import { HISTORY_COLUMNS, useSessionGrid } from '../components/sessionGridColumns.ts';
import { VirtualRows } from '../components/VirtualRows.tsx';
import { useWidth } from '../lib/columns.ts';
import { shortId } from '../lib/format.ts';
import { useDocumentTitle, useUrlState } from '../router/router.ts';
import { useUiStore } from '../state/uiStore.ts';
import t from '../components/tables.module.css';
import view from './view.module.css';

// TH3: the history browser. Q2 — *"it was bad at 21:04, show me"* — is
// unanswerable in a browser today, because nothing before the tab opened
// exists. This is where it becomes answerable.
//
// **Every filter, sort and page is server-side** (UD4). Shipping 30 days of
// rollups to a browser to filter them there is the same category error as
// returning 80 fields to a model, and the request shape is what a test asserts.
//
// R36 (docs/86): the columns fit their content and the operator can resize
// them (TU2); a range older than the service says "data since …" beside the
// range instead of raising a banner (TU4).

const RANGES: Array<{ label: string; ms: number }> = [
  { label: '1h', ms: 3600_000 },
  { label: '6h', ms: 6 * 3600_000 },
  { label: '24h', ms: 24 * 3600_000 },
  { label: '7d', ms: 7 * 24 * 3600_000 },
  { label: '30d', ms: 30 * 24 * 3600_000 },
];

const ROW_HEIGHT = 30;

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
  const setFoundKey = useUiStore((s) => s.setFoundKey);

  const rangeMs = RANGES.find((r) => r.label === range)?.ms ?? 24 * 3600_000;
  // The Find box scopes the browser rather than merely highlighting a live row.
  const effectiveBroadcast = broadcast || foundKey || '';
  const [fromMs, setFromMs] = useState(() => Date.now() - rangeMs);

  const params = useMemo<HistoryParams>(
    () => ({
      fromMs,
      role: role || undefined,
      verdict: verdict || undefined,
      broadcast: effectiveBroadcast || undefined,
      appVersion: appVersion || undefined,
      hasFindings: findingsOnly === '1' ? true : undefined,
      distrusted: distrusted === '1' ? true : undefined,
      sort,
      limit: 200,
    }),
    [fromMs, role, verdict, effectiveBroadcast, appVersion, findingsOnly, distrusted, sort],
  );

  useEffect(() => {
    setFromMs(Date.now() - rangeMs);
  }, [rangeMs]);

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

  // One data load = one set of query params. Paging appends to the load and
  // does not re-measure: the widths a reader is looking at never move.
  const loadKey = JSON.stringify(params);
  const [frame, width] = useWidth();
  const columns = useSessionGrid('history', rows, loadKey, width);

  const counts = useMemo(() => {
    const c: Record<Severity, number> = { bad: 0, warn: 0, ok: 0, unknown: 0 };
    for (const r of rows) c[r.severity] += 1;
    return c;
  }, [rows]);


  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>History</h1>
        <span className={view.subtitle}>
          {page ? (
            <>
              {page.total} session{page.total === 1 ? '' : 's'}
              {(['bad', 'warn', 'ok', 'unknown'] as const)
                .filter((s) => counts[s] > 0)
                .map((s) => ` · ${counts[s]} ${s}`)
                .join('')}
              {rows.length < page.total ? ` (first ${rows.length} loaded)` : ''}
            </>
          ) : (
            '…'
          )}
        </span>
        <span className={view.spacer} />
        <FindStream />
      </header>

      <div className={view.toolbar}>
        <div className={view.seg} role="group" aria-label="Range">
          {RANGES.map((r) => (
            <button key={r.label} type="button" aria-pressed={range === r.label} onClick={() => setRange(r.label)}>
              {r.label}
            </button>
          ))}
        </div>
        <DataSince coverage={page?.coverage} fromMs={fromMs} />
        <label className={view.chip}>
          role
          <select value={role} onChange={(e) => setRole(e.target.value)} aria-label="role">
            <option value="">any</option>
            <option value="viewer">viewer</option>
            <option value="broadcaster">broadcaster</option>
          </select>
        </label>
        <label className={view.chip}>
          verdict
          <select value={verdict} onChange={(e) => setVerdict(e.target.value)} aria-label="verdict">
            <option value="">any</option>
            <option value="bad">bad</option>
            <option value="warn">warn</option>
            <option value="ok">ok</option>
            <option value="unknown">unknown</option>
          </select>
        </label>
        <label className={view.chip}>
          version
          <input value={appVersion} placeholder="any" onChange={(e) => setAppVersion(e.target.value)} aria-label="app version" />
        </label>
        <button
          type="button"
          className={`${view.button} ${findingsOnly === '1' ? view.buttonOn : ''}`}
          aria-pressed={findingsOnly === '1'}
          onClick={() => setFindingsOnly(findingsOnly === '1' ? '' : '1')}
        >
          has findings
        </button>
        <button
          type="button"
          className={`${view.button} ${distrusted === '1' ? view.buttonOn : ''}`}
          aria-pressed={distrusted === '1'}
          onClick={() => setDistrusted(distrusted === '1' ? '' : '1')}
        >
          distrusted
        </button>
        {effectiveBroadcast && (
          <span className={view.chip}>
            broadcast <b className={view.mono}>{shortId(effectiveBroadcast)}</b>
            <button
              type="button"
              className={view.chipX}
              aria-label="Clear the broadcast filter"
              onClick={() => {
                setBroadcast('');
                setFoundKey(null);
              }}
            >
              ×
            </button>
          </span>
        )}
        <span className={view.spacer} />
        <label className={view.control}>
          sort
          <select className={view.select} value={sort} onChange={(e) => setSort(e.target.value)}>
            <option value="start">newest</option>
            <option value="severity">worst first</option>
            <option value="duration">longest</option>
            <option value="stalls">most stalls</option>
          </select>
        </label>
      </div>

      <CoverageNote coverage={page?.coverage} />
      {error && <p className={view.error}>{error}</p>}

      <section className={`${view.panel} ${view.panelClip}`}>
        {rows.length === 0 && !loading ? (
          <p className={`${view.note} ${view.panelB}`}>
            No sessions in this range.{' '}
            {page?.coverage.reason === 'none'
              ? 'This service has nothing stored at all yet — the range is unanswerable, not empty.'
              : page?.coverage.reason === 'before-data'
                ? 'Nothing is missing: the service has only been collecting since the date beside the range.'
                : 'The range is fully covered — this really is nothing rather than nothing kept.'}
          </p>
        ) : (
          <div className={t.wrap} ref={frame}>
            <div style={{ minWidth: columns.minTotal }} role="table" aria-label="sessions">
              <GridHead columns={columns} />
              <VirtualRows
                rows={rows}
                rowHeight={ROW_HEIGHT}
                height={Math.min(720, Math.max(120, rows.length * ROW_HEIGHT + 2))}
                keyOf={(r) => r.sessionId}
                ariaLabel="sessions"
                onEndReached={loadMore}
                renderRow={(r) => <GridRow row={r} columns={columns} />}
              />
            </div>
            <DragLine columns={columns} cols={HISTORY_COLUMNS} />
          </div>
        )}
        <div className={t.foot}>
          <span>
            {loading
              ? 'Loading…'
              : page
                ? rows.length < page.total
                  ? `${rows.length} of ${page.total} · scroll for more`
                  : `${rows.length} of ${page.total} · the whole range is shown`
                : ''}
          </span>
          <span className={view.spacer} />
          <WidthsNote columns={columns} cols={HISTORY_COLUMNS} />
        </div>
      </section>
    </div>
  );
}
