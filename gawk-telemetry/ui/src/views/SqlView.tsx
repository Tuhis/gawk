import { useEffect, useMemo, useState } from 'react';

import { fetchQueryStatus, runQuery } from '../api/client.ts';
import type { QueryResult, QueryStatus } from '../api/types.ts';
import { TimeChart, type Series } from '../charts/TimeChart.tsx';
import { download, seriesToCsv } from '../lib/export.ts';
import { useDocumentTitle, useUrlState } from '../router/router.ts';
import styles from './SqlView.module.css';
import view from './view.module.css';

// TH10 / UD18: the SQL console, on by default — with its cost paid honestly.
//
// Two states this view must keep apart, because conflating them is how an ops
// tool teaches its operator to distrust it:
//
//   * **"This deployment has no engine."** `go build ./...` on a fresh clone is
//     cgo-free by design (§8 Q1), so the binary carries no DuckDB. That is a
//     deployment fact, and it renders as prose — not as a broken editor and not
//     as a failed query.
//   * **"Your query was wrong."** A refusal (the allowlist) and a syntax error
//     both come back with the server's own words.
//
// Results feed the chart component, so an ad-hoc query is PLOTTABLE rather than
// a table of numbers — which is most of why the console is worth having beside
// the purpose-built views rather than instead of them.

const EXAMPLES = [
  {
    label: 'worst sessions, last 7 days',
    sql: `SELECT sessionId, role, browser, stalls, longestStallMs
FROM rollups
WHERE startedAt > epoch_ms(now()) - 7*24*3600*1000
ORDER BY stalls DESC
LIMIT 20`,
  },
  {
    label: 'median received fps by app version',
    sql: `SELECT appVersion, count(*) AS sessions,
       median(series->'receivedFps'->>'median') AS medianFps
FROM rollups
WHERE role = 'viewer'
GROUP BY appVersion
ORDER BY sessions DESC`,
  },
  {
    label: 'what a session actually reported',
    sql: `SELECT tMs, stats
FROM sessions
WHERE kind = 'sample' AND sessionId = '<paste a session id>'
ORDER BY tMs
LIMIT 50`,
  },
];

export function SqlView() {
  useDocumentTitle('sql — gawk telemetry');
  const [status, setStatus] = useState<QueryStatus | null>(null);
  const [sql, setSql] = useUrlState('q');
  const [draft, setDraft] = useState(sql);
  const [result, setResult] = useState<QueryResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [chartX, setChartX] = useUrlState('x');
  const [chartY, setChartY] = useUrlState('y');

  useEffect(() => {
    fetchQueryStatus()
      .then(setStatus)
      .catch(() => setStatus({ enabled: false, reason: 'the query surface is not available on this backend', rowLimit: 0, timeoutMs: 0 }));
  }, []);

  useEffect(() => {
    setDraft(sql);
  }, [sql]);

  const run = async (text: string) => {
    if (!text.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const res = await runQuery(text);
      setResult(res);
    } catch (e) {
      // The server's own words. "HTTP 400" would throw away the readable
      // message a console has to have.
      setError(e instanceof Error ? e.message : String(e));
      setResult(null);
    } finally {
      setBusy(false);
    }
  };

  const chart = useMemo<Series[]>(() => {
    if (!result || !chartX || !chartY) return [];
    const xi = result.columns.indexOf(chartX);
    const yi = result.columns.indexOf(chartY);
    if (xi < 0 || yi < 0) return [];
    const data = result.rows
      .map((r) => [toMs(r[xi]), toNumber(r[yi])] as [number | null, number | null])
      .filter((p): p is [number, number | null] => p[0] !== null)
      .sort((a, b) => a[0] - b[0]);
    return [{ key: chartY, label: chartY, data }];
  }, [result, chartX, chartY]);

  if (status && !status.enabled) {
    return (
      <div className={view.page}>
        <h1 className={view.title}>SQL</h1>
        <p className={view.rollupOnly}>
          <strong>No query engine is compiled into this build.</strong> {status.reason}
        </p>
        <p className={view.note}>
          This is a build property, not a failure and not a setting: a fresh clone builds
          cgo-free and the deployed image is compiled with <code>-tags duckdb</code>. Everything the
          console would query is reachable through the purpose-built views meanwhile.
        </p>
        {status.views && status.views.length > 0 && (
          <section className={view.panel}>
            <h2 className={view.panelTitle}>What would be queryable</h2>
            <ul className={styles.views}>
              {status.views.map((v) => (
                <li key={v.name}>
                  <code>{v.name}</code> — {v.description}
                </li>
              ))}
            </ul>
          </section>
        )}
      </div>
    );
  }

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>SQL</h1>
        <span className={view.subtitle}>
          read-only · {status?.rowLimit ?? 0} row cap · {Math.round((status?.timeoutMs ?? 0) / 1000)} s
          timeout
        </span>
        <span className={view.spacer} />
        {result && (
          <button
            type="button"
            className={view.button}
            onClick={() =>
              download(
                'query.csv',
                seriesToCsv(result.columns, result.rows.map((r) => r.map(cell))),
                'text/csv',
              )
            }
          >
            export csv
          </button>
        )}
      </header>

      <div className={view.controls}>
        {EXAMPLES.map((ex) => (
          <button
            key={ex.label}
            type="button"
            className={view.button}
            onClick={() => {
              setDraft(ex.sql);
              setSql(ex.sql);
              void run(ex.sql);
            }}
          >
            {ex.label}
          </button>
        ))}
      </div>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSql(draft);
          void run(draft);
        }}
      >
        <textarea
          className={styles.editor}
          value={draft}
          spellCheck={false}
          rows={7}
          placeholder="SELECT … FROM rollups"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault();
              setSql(draft);
              void run(draft);
            }
          }}
        />
        <div className={view.controls}>
          <button type="submit" className={view.button} disabled={busy}>
            {busy ? 'running…' : 'run (⌘⏎)'}
          </button>
          {result && (
            <span className={view.dim}>
              {result.rowCount} row{result.rowCount === 1 ? '' : 's'} in {result.elapsedMs} ms
              {result.truncated ? ' · truncated at the row cap' : ''}
            </span>
          )}
        </div>
      </form>

      {error && <p className={view.error}>{error}</p>}

      {result && result.rows.length > 0 && (
        <>
          <div className={view.controls}>
            <label className={view.control}>
              chart x
              <select className={view.select} value={chartX} onChange={(e) => setChartX(e.target.value)}>
                <option value="">—</option>
                {result.columns.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </label>
            <label className={view.control}>
              y
              <select className={view.select} value={chartY} onChange={(e) => setChartY(e.target.value)}>
                <option value="">—</option>
                {result.columns.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {chart.length > 0 && (
            <TimeChart
              series={chart}
              height={220}
              showZoom
              note="Charted from the result set: the x column is read as a timestamp (epoch ms or an ISO string)."
              ariaLabel="query result"
            />
          )}

          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  {result.columns.map((c) => (
                    <th key={c}>{c}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {result.rows.map((row, i) => (
                  <tr key={i}>
                    {row.map((v, j) => (
                      <td key={j} className="tnum">
                        {cell(v)}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

function cell(v: unknown): string {
  if (v === null || v === undefined) return '';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

function toNumber(v: unknown): number | null {
  const n = typeof v === 'number' ? v : Number(v);
  return Number.isFinite(n) ? n : null;
}

/** Read a cell as a timestamp: epoch ms, epoch seconds, or an ISO string. */
function toMs(v: unknown): number | null {
  if (typeof v === 'number') return v > 1e11 ? v : v * 1000;
  if (typeof v === 'string') {
    const parsed = Date.parse(v);
    if (Number.isFinite(parsed)) return parsed;
    const n = Number(v);
    if (Number.isFinite(n)) return n > 1e11 ? n : n * 1000;
  }
  return null;
}
