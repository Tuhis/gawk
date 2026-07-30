import { useCallback, useEffect, useMemo, useState } from 'react';

import {
  fetchCompare,
  fetchDiagnose,
  fetchDips,
  fetchSession,
} from '../api/client.ts';
import type { Comparison, DipReport, FieldDoc, Report, Timeline } from '../api/types.ts';
import { TimeChart, type Band, type Marker, type Series } from '../charts/TimeChart.tsx';
import { AnnotationPanel } from '../components/Annotations.tsx';
import { useAnnotations } from '../lib/useAnnotations.ts';
import { CoverageNote } from '../components/Chrome.tsx';
import { DipPanel } from '../components/DipPanel.tsx';
import { ReportPanel } from '../components/Findings.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { absoluteTime, dur, EMPTY, rangeLabel, timeZoneLabel } from '../lib/format.ts';
import { download, sessionBundle, timelineToCsv } from '../lib/export.ts';
import { href, isSessionId, useDocumentTitle, useUrlState } from '../router/router.ts';
import { fieldDoc, useMetaStore } from '../state/metaStore.ts';
import { useLiveStore } from '../state/liveStore.ts';
import styles from './view.module.css';

// TH2: the page the dead link has been pointing at.
//
// `readapi.Diagnose` has always written `#/session/<id>` into every rollup
// row's stored verdict, and rollups are permanent — so this page is not a new
// destination, it is the one that was already being promised. Everything known
// about one session, **from disk**: full resolution, from its first sample, for
// a live session as well as an ended one (docs/36 §1.1).
//
// What that finding bought is worth restating, because it is why this page can
// exist at all: `store.ReadSession` flushes the open writer, so the
// full-resolution timeline of a session happening RIGHT NOW is already on disk
// and already served. The ten-minute client-side accumulation this replaces was
// never necessary.

/** How often a live session's detail refreshes. Matched to the fleet cadence. */
const REFRESH_MS = 5000;

interface Props {
  sessionId: string;
}

export function SessionView({ sessionId }: Props) {
  const [timeline, setTimeline] = useState<Timeline | null>(null);
  const [report, setReport] = useState<Report | null>(null);
  const [compare, setCompare] = useState<Comparison | null>(null);
  const [dips, setDips] = useState<DipReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [full, setFull] = useUrlState('full');
  const paused = useLiveStore((s) => s.paused);
  const fields = useMetaStore((s) => s.fields);
  const meta = useMetaStore((s) => s.meta);
  const { notes, reload: reloadNotes } = useAnnotations({ sessionId });

  useDocumentTitle(`session ${sessionId.slice(0, 8)} — gawk telemetry`);

  const load = useCallback(
    (signal?: AbortSignal) => {
      if (!isSessionId(sessionId)) {
        setError('not a session id');
        setLoading(false);
        return;
      }
      fetchSession(sessionId, { full: full === '1', points: 600 }, signal)
        .then((tl) => {
          setTimeline(tl);
          setError(null);
        })
        .catch((e: unknown) => {
          if (signal?.aborted) return;
          setError(describe(e));
        })
        .finally(() => setLoading(false));
    },
    [sessionId, full],
  );

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    load(ac.signal);
    // The verdict, the comparison and the dips are per-click rather than
    // per-tick: they are analyses of the whole session, and re-running them
    // every five seconds would spend I/O to redraw the same sentence.
    fetchDiagnose(sessionId, ac.signal).then(setReport).catch(() => setReport(null));
    fetchCompare(sessionId, undefined, ac.signal).then(setCompare).catch(() => setCompare(null));
    fetchDips(sessionId, undefined, ac.signal).then(setDips).catch(() => setDips(null));
    return () => ac.abort();
  }, [sessionId, load]);

  // A live session keeps updating. Paused freezes it, like everything else.
  //
  // The server SAYS whether it is live; `endedAtMs` cannot answer it, because
  // a session that supplies no end of its own gets the last receive time as
  // one — so every session, open or finished, comes back with an end.
  const live = timeline?.live ?? false;
  useEffect(() => {
    if (!live || paused) return;
    const id = setInterval(() => load(), REFRESH_MS);
    return () => clearInterval(id);
  }, [live, paused, load]);

  const model = useMemo(() => (timeline ? buildCharts(timeline, fields) : null), [timeline, fields]);

  if (error) {
    return (
      <div className={styles.page}>
        <p className={styles.error}>
          {error === 'not found'
            ? 'No such session. It may never have reported, or its id may be mistyped — nothing was pruned to produce this.'
            : error === 'not a session id'
              ? `“${sessionId}” is not a session id (24 hex characters).`
              : error}
        </p>
        <p className={styles.note}>
          <a href={href('history')}>Browse history →</a>
        </p>
      </div>
    );
  }
  if (loading || !timeline || !model) return <p className={styles.loading}>Reading the session…</p>;

  const startedAtMs = timeline.startedAtMs ?? 0;
  const endedAtMs = timeline.endedAtMs ?? Date.now();
  const cfg = timeline.config ?? {};
  // UD10: a session whose raw window was pruned reads as rollup-only, never as
  // an empty chart.
  const rollupOnly =
    timeline.totalSamples === 0 &&
    meta !== null &&
    meta.rawFromMs > 0 &&
    startedAtMs > 0 &&
    startedAtMs < meta.rawFromMs;

  const markers: Marker[] = (timeline.events ?? []).map((e) => ({
    atMs: startedAtMs + (e.tMs ?? 0),
    label: e.event ?? e.kind,
  }));
  for (const n of notes) {
    if (n.atMs) markers.push({ atMs: n.atMs, label: `note: ${n.text.slice(0, 24)}`, color: '#58a6ff' });
  }

  return (
    <div className={styles.page}>
      <header className={styles.head}>
        <SeverityBadge severity={report ? worstSeverity(report) : 'unknown'} />
        <h1 className={styles.title}>{timeline.role}</h1>
        <span className={styles.subtitle}>{sessionId}</span>
        {timeline.broadcastKey && (
          <a className={styles.subtitle} href={href('broadcast', timeline.broadcastKey)}>
            broadcast {timeline.broadcastKey} →
          </a>
        )}
        <span className={styles.spacer} />
        <div className={styles.controls}>
          <button
            type="button"
            className={`${styles.button} ${full === '1' ? styles.buttonOn : ''}`}
            onClick={() => setFull(full === '1' ? '' : '1')}
            title="Every stored sample, rather than an envelope-preserving reduction"
          >
            full resolution
          </button>
          <button
            type="button"
            className={styles.button}
            onClick={() => download(`session-${sessionId}.json`, sessionBundle(timeline, report, notes), 'application/json')}
          >
            export json
          </button>
          <button
            type="button"
            className={styles.button}
            onClick={() => download(`session-${sessionId}.csv`, timelineToCsv(timeline), 'text/csv')}
          >
            export csv
          </button>
        </div>
      </header>

      <p className={styles.note}>
        {rangeLabel(startedAtMs, endedAtMs)} {timeZoneLabel(startedAtMs)} · {dur(endedAtMs - startedAtMs)}
        {live ? ' · still running' : ''} · {timeline.totalSamples} samples
        {timeline.clockOffsetMs
          ? ` · this client’s clock is ${dur(Math.abs(timeline.clockOffsetMs))} ${
              timeline.clockOffsetMs > 0 ? 'behind' : 'ahead of'
            } the service’s (measured, and applied on shared axes)`
          : ''}
      </p>

      {rollupOnly && (
        <p className={styles.rollupOnly}>
          Raw samples for this session have been pruned — it started before the{' '}
          {meta?.retentionDays}-day boundary. The permanent rollup and its stored verdict remain
          below; the charts are empty because nothing was kept, not because nothing happened.
        </p>
      )}
      {timeline.truncated && <p className={styles.distrust}>{timeline.note}</p>}

      <section className={styles.panel}>
        <h2 className={styles.panelTitle}>Identity and configuration</h2>
        <div className={styles.grid}>
          <KV k="role" v={timeline.role} />
          <KV k="started" v={`${absoluteTime(startedAtMs)}`} />
          <KV k="samples" v={String(timeline.totalSamples)} />
          {Object.entries(cfg).map(([k, v]) => (
            <KV key={k} k={k} v={v} />
          ))}
        </div>
      </section>

      {report && (
        <section>
          <h2 className={styles.panelTitle}>Verdict</h2>
          <ReportPanel
            report={report}
            sessionId={sessionId}
            fromMs={startedAtMs}
            toMs={endedAtMs}
            title={`session ${sessionId}`}
          />
        </section>
      )}

      {compare && Object.keys(compare.deltas).length > 0 && (
        <section className={styles.panel}>
          <h2 className={styles.panelTitle}>Against the fleet median for {compare.class}</h2>
          <div className={styles.grid}>
            {Object.entries(compare.deltas).map(([k, d]) => (
              <KV
                key={k}
                k={k}
                v={`${d.session.toFixed(1)} vs ${d.fleet.toFixed(1)}${
                  d.ratio ? ` (${(d.ratio * 100).toFixed(0)}%)` : ''
                }`}
              />
            ))}
          </div>
          {compare.note && <p className={styles.note}>{compare.note}</p>}
        </section>
      )}

      <section className={styles.panel}>
        <h2 className={styles.panelTitle}>Timeline</h2>
        {/* Every stored event is a labelled marker at its own timestamp —
            close codes, reconnects, config applied, resync. `Timeline.Events`
            has been returned since R28 and nothing rendered it. */}
        {model.charts.map((c) => (
          <div key={c.title} className={styles.chartRow}>
            <div className={styles.chartTitle}>
              <strong>{c.title}</strong>
              <span>{c.unit}</span>
            </div>
            <TimeChart
              series={c.series}
              markers={markers}
              bands={model.hiddenBands}
              fromMs={startedAtMs}
              toMs={endedAtMs}
              unit={c.unit}
              group={`session-${sessionId}`}
              showZoom={c === model.charts[model.charts.length - 1]}
              note={c === model.charts[0] ? timeline.note : undefined}
              empty={rollupOnly ? 'Raw samples were pruned — rollup only.' : 'No samples in range.'}
              ariaLabel={`${c.title} over the session`}
            />
          </div>
        ))}
        {model.hiddenBands.length > 0 && (
          <p className={styles.note}>
            Shaded spans are where this tab was in the background: rendering stops there while
            decode carries on, so a collapsed rendered rate inside one is tab state, not the stream.
          </p>
        )}
      </section>

      {dips && <DipPanel report={dips} />}

      <AnnotationPanel
        sessionId={sessionId}
        atMs={startedAtMs}
        notes={notes}
        onChange={reloadNotes}
      />

      <CoverageNote coverage={undefined} />
    </div>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className={styles.kv}>
      <span className={styles.k}>{k}</span>
      <span className={styles.v}>{v || EMPTY}</span>
    </div>
  );
}

interface ChartSpec {
  title: string;
  unit: string;
  series: Series[];
}

/**
 * Group the returned fields into charts by UNIT, using the catalogue.
 *
 * Grouping by unit rather than by a hard-coded pair list is what makes this
 * survive a new field: `SessionTimeline` had two hard-coded chart pairs per
 * role, so a field added to the Go tables could never appear here. The
 * catalogue says what a field IS; this arranges whatever it is given.
 */
function buildCharts(tl: Timeline, fields: FieldDoc[]): { charts: ChartSpec[]; hiddenBands: Band[] } {
  const start = tl.startedAtMs ?? 0;
  const at = (p: Record<string, number>) => start + (p.tMs ?? 0);
  const byUnit = new Map<string, Series[]>();

  for (const name of tl.fields) {
    if (name === 'tMs') continue;
    const doc = fieldDoc(fields, name);
    const unit = doc?.unit ?? '';
    // A gap is a BREAK: a point where the field was absent contributes `null`,
    // which ECharts draws as a discontinuity rather than a glide to the next
    // value (UD9). Filtering the point out instead would silently close it.
    const data: Array<[number, number | null]> = tl.points.map((p) => [
      at(p),
      p[name] === undefined ? null : p[name],
    ]);
    if (data.every(([, v]) => v === null)) continue;
    const list = byUnit.get(unit) ?? [];
    list.push({ key: name, label: name, data, unit });
    byUnit.set(unit, list);
  }

  const charts: ChartSpec[] = [];
  for (const [unit, series] of byUnit) {
    charts.push({ title: unit ? `by ${unit}` : 'unitless', unit, series });
  }
  // fps first: it is the rate every verdict is about.
  charts.sort((a, b) => (a.unit === 'fps' ? -1 : b.unit === 'fps' ? 1 : a.unit.localeCompare(b.unit)));

  // Hidden-tab spans, folded from the boolean the same way the live view
  // shades them.
  const hiddenBands: Band[] = [];
  let runStart: number | null = null;
  for (const p of tl.points) {
    const on = p.documentHidden === 1;
    if (on && runStart === null) runStart = at(p);
    if (!on && runStart !== null) {
      hiddenBands.push({ fromMs: runStart, toMs: at(p), label: 'tab hidden' });
      runStart = null;
    }
  }
  if (runStart !== null && tl.points.length) {
    hiddenBands.push({
      fromMs: runStart,
      toMs: at(tl.points[tl.points.length - 1]),
      label: 'tab hidden',
    });
  }
  return { charts, hiddenBands };
}

function worstSeverity(rep: Report) {
  if (rep.findings?.length) return rep.findings[0].severity;
  return rep.passed?.length ? 'ok' : 'unknown';
}

function describe(e: unknown): string {
  if (e && typeof e === 'object' && 'status' in e) {
    const status = (e as { status: number }).status;
    if (status === 404) return 'not found';
    if (status === 400) return 'not a session id';
  }
  return e instanceof Error ? e.message : String(e);
}
