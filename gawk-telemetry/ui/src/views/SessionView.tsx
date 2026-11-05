import { useCallback, useEffect, useMemo, useState } from 'react';

import { fetchCompare, fetchDiagnose, fetchDips, fetchSession } from '../api/client.ts';
import type { Comparison, DipReport, FieldDoc, Report, Severity, Timeline } from '../api/types.ts';
import { usePalette } from '../charts/palette.ts';
import { cursorLabel, lastValue, readoutValue, useStack, valueAt, type Stack } from '../charts/stack.ts';
import { StackChart, Y_GUTTER, type StackBand, type StackMark, type StackSeries } from '../charts/StackChart.tsx';
import { TimeAxis, ZoomBar } from '../charts/TimeAxis.tsx';
import { AnnotationPanel } from '../components/Annotations.tsx';
import { DipTable } from '../components/DipPanel.tsx';
import { ReportPanel } from '../components/Findings.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { absoluteTime, compactTime, dur, EMPTY, num, rangeLabel, timeZoneLabel } from '../lib/format.ts';
import { download, sessionBundle, timelineToCsv } from '../lib/export.ts';
import { useAnnotations } from '../lib/useAnnotations.ts';
import { href, isSessionId, useDocumentTitle, useUrlState } from '../router/router.ts';
import { fieldDoc, useMetaStore } from '../state/metaStore.ts';
import { useLiveStore } from '../state/liveStore.ts';
import { usePinFollow } from '../state/usePinFollow.ts';
import styles from './SessionView.module.css';
import view from './view.module.css';

// TH2: the page the dead link has been pointing at.
//
// `readapi.Diagnose` has always written `#/session/<id>` into every rollup
// row's stored verdict, and rollups are permanent — so this page is not a new
// destination, it is the one that was already being promised. Everything known
// about one session, **from disk**: full resolution, from its first sample, for
// a live session as well as an ended one (docs/36 §1.1).
//
// R36 (docs/86 TU8): two columns — the verdict, the timeline and the dips on
// the left; who the session is, how it compares and the notes on the right —
// where R31 stacked everything into one ~5 900 px column. The timeline is a
// chart STACK (one crosshair, one zoom, one time axis) whose charts are titled
// by the question they answer, with each series' value at the crosshair in
// the chart's own legend. Dips are one table row per episode.

/** How often a live session's detail refreshes. Matched to the fleet cadence. */
const REFRESH_MS = 5000;

/** Charts open by default; the rest are one click away. */
const OPEN_CHARTS = 4;

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

  if (error) {
    return (
      <div className={view.page}>
        <p className={view.error}>
          {error === 'not found'
            ? 'No such session. It may never have reported, or its id may be mistyped — nothing was pruned to produce this.'
            : error === 'not a session id'
              ? `“${sessionId}” is not a session id (24 hex characters).`
              : error}
        </p>
        <p className={view.note}>
          <a href={href('history')}>Browse history →</a>
        </p>
      </div>
    );
  }
  if (loading || !timeline) return <p className={view.loading}>Reading the session…</p>;

  const startedAtMs = timeline.startedAtMs ?? 0;
  const endedAtMs = timeline.endedAtMs ?? Date.now();
  // UD10: a session whose raw window was pruned reads as rollup-only, never as
  // an empty chart.
  const rollupOnly =
    timeline.totalSamples === 0 && meta !== null && meta.rawFromMs > 0 && startedAtMs > 0 && startedAtMs < meta.rawFromMs;
  const severity: Severity = report ? worstSeverity(report) : 'unknown';

  return (
    <div className={view.page}>
      <div className={view.crumbs}>
        <a href={href('history')}>History</a>
        <span>/</span>
        <span>session</span>
      </div>
      <header className={view.head}>
        <SeverityBadge severity={severity} />
        <h1 className={view.title}>
          {capitalise(timeline.role)} <span className={view.titleId}>{sessionId}</span>
        </h1>
        {timeline.broadcastKey && (
          <a className={view.tag} href={href('broadcast', timeline.broadcastKey)} title="Every participant on one axis">
            broadcast {timeline.broadcastKey.slice(0, 8)} →
          </a>
        )}
        {timeline.roomKey && (
          // R42 (RM8): a session that started inside a room says so, and the
          // chip is the way to every other session of that room.
          <a className={view.tag} href={href('room', timeline.roomKey)} title="Every session of this room, grouped by broadcast">
            room {timeline.roomKey.slice(0, 8)} →
          </a>
        )}
        {live && (
          <span className={styles.live}>
            <i aria-hidden />
            LIVE
          </span>
        )}
        <span className={view.spacer} />
        <button
          type="button"
          className={`${view.button} ${full === '1' ? view.buttonOn : ''}`}
          aria-pressed={full === '1'}
          onClick={() => setFull(full === '1' ? '' : '1')}
          title="Every stored sample, rather than an envelope-preserving reduction"
        >
          full resolution
        </button>
        <button
          type="button"
          className={view.button}
          onClick={() => download(`session-${sessionId}.json`, sessionBundle(timeline, report, notes), 'application/json')}
        >
          export json
        </button>
        <button
          type="button"
          className={view.button}
          onClick={() => download(`session-${sessionId}.csv`, timelineToCsv(timeline), 'text/csv')}
        >
          export csv
        </button>
      </header>
      <p className={`${view.note} ${styles.span}`}>
        {rangeLabel(startedAtMs, endedAtMs)} {timeZoneLabel(startedAtMs)} · {dur(endedAtMs - startedAtMs)}
        {live ? ' · still running' : ''} · {timeline.totalSamples.toLocaleString()} samples
        {timeline.clockOffsetMs
          ? ` · this client’s clock is ${dur(Math.abs(timeline.clockOffsetMs))} ${
              timeline.clockOffsetMs > 0 ? 'behind' : 'ahead of'
            } the service’s (measured, and applied on shared axes)`
          : ''}
      </p>

      {rollupOnly && (
        <p className={view.rollupOnly}>
          Raw samples for this session have been pruned — it started before the {meta?.retentionDays}-day boundary. The
          permanent rollup and its stored verdict remain; the charts are empty because nothing was kept, not because
          nothing happened.
        </p>
      )}
      {timeline.truncated && <p className={view.distrust}>{timeline.note}</p>}

      <div className={styles.cols}>
        <div className={styles.main}>
          {report && (
            <ReportPanel
              report={report}
              sessionId={sessionId}
              fromMs={startedAtMs}
              toMs={endedAtMs}
              title={`session ${sessionId}`}
            />
          )}
          <TimelinePanel
            timeline={timeline}
            fields={fields}
            startedAtMs={startedAtMs}
            endedAtMs={endedAtMs}
            rollupOnly={rollupOnly}
            notes={notes}
            dips={dips}
          />
          {dips && <DipTable report={dips} />}
        </div>

        <aside className={styles.side}>
          <Identity timeline={timeline} startedAtMs={startedAtMs} />
          {compare && Object.keys(compare.deltas).length > 0 && <FleetComparison compare={compare} />}
          <AnnotationPanel
            sessionId={sessionId}
            atMs={startedAtMs}
            followPin
            notes={notes}
            onChange={reloadNotes}
          />
        </aside>
      </div>
    </div>
  );
}

function TimelinePanel({
  timeline,
  fields,
  startedAtMs,
  endedAtMs,
  rollupOnly,
  notes,
  dips,
}: {
  timeline: Timeline;
  fields: FieldDoc[];
  startedAtMs: number;
  endedAtMs: number;
  rollupOnly: boolean;
  notes: ReturnType<typeof useAnnotations>['notes'];
  dips: DipReport | null;
}) {
  const model = useMemo(() => buildCharts(timeline, fields), [timeline, fields]);
  const stack = useStack(startedAtMs, endedAtMs);
  const [showAll, setShowAll] = useState(false);
  const group = `session-${timeline.sessionId}`;
  usePinFollow(stack.cursorMs);

  const bands = useMemo<StackBand[]>(
    () => [
      ...model.hidden.map((b) => ({ ...b, kind: 'veil' as const, label: 'tab hidden' })),
      ...(dips?.episodes ?? []).map((e) => ({ fromMs: e.fromMs, toMs: e.toMs, kind: 'dip' as const, label: 'dip' })),
    ],
    [model.hidden, dips],
  );
  const marks = useMemo<StackMark[]>(
    () => notes.filter((n) => n.atMs).map((n) => ({ atMs: n.atMs!, kind: 'note', label: `note: ${n.text}` })),
    [notes],
  );
  const ticks = useMemo(
    () => (timeline.events ?? []).map((e) => startedAtMs + (e.tMs ?? 0)),
    [timeline.events, startedAtMs],
  );

  const charts = showAll ? model.charts : model.charts.slice(0, OPEN_CHARTS);
  const hidden = model.charts.length - charts.length;
  const event = stack.cursorMs === null ? undefined : nearestEvent(timeline, startedAtMs, stack.cursorMs, model.cadenceMs);

  return (
    <section className={view.panel} aria-label="Timeline">
      <div className={view.panelH}>
        <h2 className={view.panelT}>Timeline</h2>
        {timeline.note && <span className={view.note}>{timeline.note}</span>}
        <span className={view.spacer} />
        {event && <span className={styles.event}>▏{event}</span>}
        <span className={view.dim}>
          crosshair{' '}
          <b className={styles.crossT}>{stack.cursorMs === null ? '—' : cursorLabel(stack.cursorMs)}</b>
        </span>
      </div>
      {model.charts.length === 0 ? (
        <p className={`${view.note} ${view.panelB}`}>
          {rollupOnly ? 'Raw samples were pruned — rollup only.' : 'No numeric samples in this session.'}
        </p>
      ) : (
        <>
          {charts.map((c) => (
            <ChartRow
              key={c.unit}
              chart={c}
              stack={stack}
              group={group}
              bands={bands}
              marks={marks}
              ticks={ticks}
              fromMs={startedAtMs}
              toMs={endedAtMs}
              cadenceMs={model.cadenceMs}
            />
          ))}
          <div className={styles.xaxis} style={{ marginLeft: 14 + Y_GUTTER }}>
            <TimeAxis viewFrom={stack.viewFrom} viewTo={stack.viewTo} cursorMs={stack.cursorMs} />
          </div>
          <div className={styles.zoom} style={{ marginLeft: 14 + Y_GUTTER }}>
            <ZoomBar fromMs={startedAtMs} toMs={endedAtMs} group={group} onInit={stack.register} />
          </div>
          <div className={styles.timelineFoot}>
            {hidden > 0 || showAll ? (
              <button type="button" className={`${view.button} ${view.buttonQuiet} ${view.buttonSmall}`} onClick={() => setShowAll((v) => !v)}>
                {showAll ? 'show the main charts only' : `show ${hidden} more chart${hidden === 1 ? '' : 's'} (${model.charts.slice(OPEN_CHARTS).map((c) => c.unit || 'flags').join(', ')})`}
              </button>
            ) : null}
            {model.hidden.length > 0 && (
              <span className={view.note}>
                Hatched spans are where this tab was in the background: rendering stops there while decode carries on,
                so a collapsed rendered rate inside one is tab state, not the stream.
              </span>
            )}
          </div>
        </>
      )}
    </section>
  );
}

function ChartRow({
  chart,
  stack,
  group,
  bands,
  marks,
  ticks,
  fromMs,
  toMs,
  cadenceMs,
}: {
  chart: ChartSpec;
  stack: Stack;
  group: string;
  bands: StackBand[];
  marks: StackMark[];
  ticks: number[];
  fromMs: number;
  toMs: number;
  cadenceMs: number;
}) {
  const p = usePalette();
  const t = stack.cursorMs;
  // Colours assigned here, from the palette, so the legend swatch and the line
  // are the same value.
  const series = useMemo<StackSeries[]>(
    () => chart.series.map((s, i) => ({ ...s, color: p.series[i % p.series.length] })),
    [chart.series, p],
  );
  return (
    <div className={styles.chart}>
      <div className={styles.ch}>
        <h3>{chart.title}</h3>
        <span className={view.dimmer}>{chart.unit}</span>
        <div className={styles.vals}>
          {series.map((s) => {
            const v = t === null ? lastValue(s.data) : valueAt(s.data, t, cadenceMs * 2.5);
            return (
              <span key={s.key}>
                <i style={{ background: s.color }} />
                {s.label} <b>{readoutValue(v)}</b>
              </span>
            );
          })}
        </div>
      </div>
      <StackChart
        series={series}
        bands={bands}
        marks={marks}
        ticks={ticks}
        fromMs={fromMs}
        toMs={toMs}
        height={chart.height}
        group={group}
        onInit={stack.register}
        yLabels
        ariaLabel={`${chart.title} (${chart.unit || 'unitless'}) over the session`}
      />
    </div>
  );
}

function Identity({ timeline, startedAtMs }: { timeline: Timeline; startedAtMs: number }) {
  const cfg = timeline.config ?? {};
  const samples = timeline.totalSamples;
  return (
    <section className={view.panel} aria-label="Identity and configuration">
      <div className={view.panelH}>
        <h2 className={view.panelT}>Identity and configuration</h2>
      </div>
      <dl className={`${view.panelB} ${styles.kv}`}>
        <dt>role</dt>
        <dd>{timeline.role}</dd>
        <dt>started</dt>
        <dd title={absoluteTime(startedAtMs)}>{compactTime(startedAtMs)}</dd>
        <dt>samples</dt>
        <dd>{samples.toLocaleString()}</dd>
        {Object.entries(cfg).map(([k, v]) => (
          <KV key={k} k={k} v={v} />
        ))}
      </dl>
    </section>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt>{k}</dt>
      <dd className={/^[0-9a-f.:\-_]+$/i.test(v) && v.length > 3 ? styles.mono : undefined}>{v || EMPTY}</dd>
    </>
  );
}

/** The session against its class's fleet median, as bars with the median marked. */
function FleetComparison({ compare }: { compare: Comparison }) {
  return (
    <section className={view.panel} aria-label="Against the fleet median">
      <div className={view.panelH}>
        <h2 className={view.panelT}>Against the fleet median</h2>
        <span className={view.note}>{compare.class}</span>
      </div>
      <div className={`${view.panelB} ${styles.compare}`}>
        {Object.entries(compare.deltas).map(([k, d]) => {
          const top = Math.max(Math.abs(d.session), Math.abs(d.fleet)) || 1;
          return (
            <div key={k} className={styles.cmp}>
              <span className={styles.cmpK}>{k}</span>
              <span>
                <b>{num(d.session, 1)}</b> <span className={view.dimmer}>vs {num(d.fleet, 1)}</span>
              </span>
              <div className={styles.track} aria-hidden>
                <div className={styles.fill} style={{ width: `${(Math.abs(d.session) / top) * 100}%` }} />
                <div className={styles.median} style={{ left: `${(Math.abs(d.fleet) / top) * 100}%` }} title="fleet median" />
              </div>
            </div>
          );
        })}
        {compare.note && <p className={view.note}>{compare.note}</p>}
      </div>
    </section>
  );
}

interface ChartSpec {
  title: string;
  unit: string;
  series: StackSeries[];
  height: number;
}

/**
 * The question a chart answers, by its unit (R36: R31 titled them "by fps").
 * A unit this table does not know falls back to naming the unit.
 */
function questionFor(unit: string, role: string): string {
  const broadcaster = role === 'broadcaster';
  switch (unit) {
    case 'fps':
      return broadcaster ? 'Were frames captured, encoded and sent?' : 'Did frames arrive, decode and render?';
    case 'ms':
      return 'How long did each step take?';
    case 'frames':
      return 'Were frames dropped or queued?';
    case 'bps':
      return 'What bitrate was asked for?';
    case 'bytes':
      return 'How many bytes moved, and how much memory was held?';
    case 'datagrams':
      return 'How many datagrams moved?';
    case 'chunks':
      return 'Were chunks lost, late or repaired?';
    case 'packets':
    case 'packets/s':
      return 'Did audio packets flow?';
    case 'streams':
      return 'Did keyframe streams land?';
    case '%':
      return 'How much was lost?';
    case 'px':
      return 'At what resolution?';
    case 'viewers':
      return 'How many were watching?';
    case 'connections':
    case 'legs':
      return 'How were the stripe legs doing?';
    case '':
      return 'Flags and states';
    default:
      return `In ${unit}`;
  }
}

const UNIT_ORDER = ['fps', 'ms', 'frames', 'datagrams', 'chunks', 'bps', '%'];

/**
 * Group the returned fields into charts by UNIT, using the catalogue.
 *
 * Grouping by unit rather than by a hard-coded pair list is what makes this
 * survive a new field: `SessionTimeline` had two hard-coded chart pairs per
 * role, so a field added to the Go tables could never appear here. The
 * catalogue says what a field IS; this arranges whatever it is given.
 */
function buildCharts(
  tl: Timeline,
  fields: FieldDoc[],
): { charts: ChartSpec[]; hidden: Array<{ fromMs: number; toMs: number }>; cadenceMs: number } {
  const start = tl.startedAtMs ?? 0;
  const at = (p: Record<string, number>) => start + (p.tMs ?? 0);
  const byUnit = new Map<string, StackSeries[]>();

  for (const name of tl.fields) {
    if (name === 'tMs') continue;
    const doc = fieldDoc(fields, name);
    const unit = doc?.unit ?? '';
    // A gap is a BREAK: a point where the field was absent contributes `null`,
    // which ECharts draws as a discontinuity rather than a glide to the next
    // value (UD9). Filtering the point out instead would silently close it.
    const data: Array<[number, number | null]> = tl.points.map((p) => [at(p), p[name] === undefined ? null : p[name]]);
    if (data.every(([, v]) => v === null)) continue;
    const list = byUnit.get(unit) ?? [];
    list.push({ key: name, label: name, data, step: doc?.semantic === 'bool' });
    byUnit.set(unit, list);
  }

  const rank = (u: string) => {
    const i = UNIT_ORDER.indexOf(u);
    return i < 0 ? UNIT_ORDER.length + (u === '' ? 1 : 0) : i;
  };
  const charts: ChartSpec[] = [...byUnit.entries()]
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
    .map(([unit, series]) => ({
      unit,
      series,
      title: questionFor(unit, tl.role),
      height: unit === 'fps' ? 132 : 96,
    }));

  // Hidden-tab spans, folded from the boolean the same way the live view
  // shades them.
  const hidden: Array<{ fromMs: number; toMs: number }> = [];
  let runStart: number | null = null;
  for (const p of tl.points) {
    const on = p.documentHidden === 1;
    if (on && runStart === null) runStart = at(p);
    if (!on && runStart !== null) {
      hidden.push({ fromMs: runStart, toMs: at(p) });
      runStart = null;
    }
  }
  if (runStart !== null && tl.points.length) hidden.push({ fromMs: runStart, toMs: at(tl.points[tl.points.length - 1]) });

  // The session's own sampling cadence: the median gap between points. A
  // readout older than a couple of these is absent at the crosshair (UD9).
  const gaps: number[] = [];
  for (let i = 1; i < tl.points.length && gaps.length < 400; i++) gaps.push(at(tl.points[i]) - at(tl.points[i - 1]));
  gaps.sort((a, b) => a - b);
  const cadenceMs = Math.max(1000, gaps.length ? gaps[gaps.length >> 1] : 2000);

  return { charts, hidden, cadenceMs };
}

function nearestEvent(tl: Timeline, start: number, t: number, within: number): string | undefined {
  let best: { name: string; d: number } | undefined;
  for (const e of tl.events ?? []) {
    const d = Math.abs(start + (e.tMs ?? 0) - t);
    if (d <= within && (!best || d < best.d)) best = { name: e.event ?? e.kind, d };
  }
  return best?.name;
}

function worstSeverity(rep: Report): Severity {
  if (rep.findings?.length) return rep.findings[0].severity;
  return rep.passed?.length ? 'ok' : 'unknown';
}

function capitalise(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

function describe(e: unknown): string {
  if (e && typeof e === 'object' && 'status' in e) {
    const status = (e as { status: number }).status;
    if (status === 404) return 'not found';
    if (status === 400) return 'not a session id';
  }
  return e instanceof Error ? e.message : String(e);
}
