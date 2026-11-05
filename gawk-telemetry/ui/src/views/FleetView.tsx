import { useEffect, useMemo, useState } from 'react';

import { fetchCohorts, fetchFleetTimeline, fetchTrends } from '../api/client.ts';
import type { Cohort, FleetTimeline, FleetTimelineRow, Trend } from '../api/types.ts';
import { timeTicks } from '../charts/stack.ts';
import { TimeChart, type Band, type Series } from '../charts/TimeChart.tsx';
import { CoverageNote, DesktopOnly } from '../components/Chrome.tsx';
import { compactTime, dur, num, shortDate, shortId } from '../lib/format.ts';
import { href, useDocumentTitle, useUrlState } from '../router/router.ts';
import styles from './FleetView.module.css';
import view from './view.module.css';

// TH7, first-class by owner decision. Two halves, answering two questions
// nothing else in the design can.
//
// **The fleet timeline** (Q5): one row per broadcast over a range, all on one
// shared axis, so a relay-wide or pod-wide event shows as a VERTICAL STRIPE
// across unrelated broadcasts. That is the only thing that distinguishes "gawk
// had a bad minute" from "that broadcast had a bad minute".
//
// **Trends** (Q9): bucketed aggregates over the PERMANENT rollups, so a range
// can far outrun the raw window — which is the point, since that is what makes
// "did R29's parity actually cut frame loss on the fleet?" answerable at all.
//
// R36 (docs/86 TU9): the stripes get gridlines and labels across the range,
// not only at its two ends; the range before the service existed is hatched
// "not installed yet" instead of raising a banner (TU4); and a trend bucket
// under five sessions is drawn as a HOLLOW point — present, claiming nothing —
// so a small fleet's trend is not a blank plot with a full legend.

const RANGES = [
  { label: '24h', ms: 24 * 3600_000 },
  { label: '7d', ms: 7 * 24 * 3600_000 },
  { label: '30d', ms: 30 * 24 * 3600_000 },
  { label: '90d', ms: 90 * 24 * 3600_000 },
];

const METRICS = [
  { value: 'receivedFps', label: 'received fps' },
  { value: 'capToRenderMs', label: 'cap→render ms' },
  { value: 'stalls', label: 'stalls' },
  { value: 'badShare', label: 'bad share' },
  { value: 'dipEpisodes', label: 'dip episodes' },
  { value: 'reconnects', label: 'reconnects' },
];

const GROUPS = [
  { value: '', label: 'all together' },
  { value: 'appVersion', label: 'app version' },
  { value: 'deliveryMode', label: 'delivery mode' },
  { value: 'browser', label: 'browser' },
  { value: 'os', label: 'os' },
  { value: 'resolution', label: 'resolution' },
];

const STATS = ['median', 'p95', 'mean'];

export function FleetView() {
  useDocumentTitle('fleet — gawk telemetry');
  const [range, setRange] = useUrlState('range', '7d');
  const [metric, setMetric] = useUrlState('metric', 'receivedFps');
  const [groupBy, setGroupBy] = useUrlState('groupBy', 'appVersion');
  const [stat, setStat] = useUrlState('stat', 'median');
  const [cohortA, setCohortA] = useUrlState('a');
  const [cohortB, setCohortB] = useUrlState('b');

  const [timeline, setTimeline] = useState<FleetTimeline | null>(null);
  const [trend, setTrend] = useState<Trend | null>(null);
  const [cohort, setCohort] = useState<Cohort | null>(null);
  const [error, setError] = useState<string | null>(null);

  const rangeMs = RANGES.find((r) => r.label === range)?.ms ?? 7 * 24 * 3600_000;

  useEffect(() => {
    const ac = new AbortController();
    const fromMs = Date.now() - rangeMs;
    Promise.all([
      fetchFleetTimeline({ fromMs }, ac.signal).then(setTimeline),
      fetchTrends({ fromMs, metric, groupBy: groupBy || undefined, stat }, ac.signal).then(setTrend),
    ]).catch((e: unknown) => {
      if (!ac.signal.aborted) setError(e instanceof Error ? e.message : String(e));
    });
    return () => ac.abort();
  }, [rangeMs, metric, groupBy, stat]);

  useEffect(() => {
    if (!cohortA || !cohortB || !groupBy) {
      setCohort(null);
      return;
    }
    const ac = new AbortController();
    fetchCohorts(
      { metric, stat, groupBy, a: cohortA, b: cohortB, aFromMs: Date.now() - rangeMs, bFromMs: Date.now() - rangeMs },
      ac.signal,
    )
      .then(setCohort)
      .catch(() => setCohort(null));
    return () => ac.abort();
  }, [cohortA, cohortB, groupBy, metric, stat, rangeMs]);

  const { trendSeries, allThin, anyPoint } = useMemo(() => {
    const list = trend?.series ?? [];
    const points = list.reduce((n, s) => n + s.points.length, 0);
    const thin = list.reduce((n, s) => n + s.points.filter((p) => p.thin).length, 0);
    const series: Series[] = list.map((s) => {
      return {
        key: s.group,
        label: `${s.group || 'all'} (${s.sessions})`,
        // A thin bucket contributes a `null` to the line, so it BREAKS rather
        // than dipping through a number computed from two sessions —
        // `FleetSummary` already refuses to over-claim below five — and the
        // same bucket is drawn hollow beside it, so the range is not mistaken
        // for empty.
        data: s.points.map((p) => [p.atMs, p.thin ? null : p.value] as [number, number | null]),
        hollow: s.points.map((p) => [p.atMs, p.thin ? p.value : null] as [number, number | null]),
        points: true,
      };
    });
    return { trendSeries: series, allThin: points > 0 && thin === points, anyPoint: points > 0 };
  }, [trend]);

  const groups = useMemo(() => (trend?.series ?? []).map((s) => s.group), [trend]);
  const viewers = timeline?.rows.reduce((n, r) => n + r.viewers, 0) ?? 0;
  const metricLabel = METRICS.find((m) => m.value === metric)?.label ?? metric;
  const bucket = trend?.bucketMs ? (trend.bucketMs >= 86_400_000 ? 'one point per day' : `one point per ${dur(trend.bucketMs)}`) : '';

  // "Not installed yet": the stretch of the range before the first rollup.
  const since = timeline?.coverage.rollupsFromMs;
  const trendBands = useMemo<Band[]>(
    () =>
      since && timeline && since > timeline.fromMs
        ? [{ fromMs: timeline.fromMs, toMs: since, kind: 'veil', label: 'service not installed yet' }]
        : [],
    [since, timeline],
  );

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Fleet</h1>
        <span className={view.subtitle}>
          {timeline
            ? `${timeline.rows.length} broadcast${timeline.rows.length === 1 ? '' : 's'} · ${viewers} viewer${viewers === 1 ? '' : 's'} · ${compactTime(timeline.fromMs)} → ${compactTime(timeline.toMs)}`
            : '…'}
        </span>
        <span className={view.spacer} />
        <div className={view.seg} role="group" aria-label="Range">
          {RANGES.map((r) => (
            <button key={r.label} type="button" aria-pressed={range === r.label} onClick={() => setRange(r.label)}>
              {r.label}
            </button>
          ))}
        </div>
      </header>

      {error && <p className={view.error}>{error}</p>}
      <CoverageNote coverage={timeline?.coverage} />
      <DesktopOnly what="The fleet timeline" />

      <section className={view.panel} aria-label="Broadcasts on one axis">
        <div className={view.panelH}>
          <h2 className={view.panelT}>Broadcasts on one axis</h2>
          <span className={view.note}>a pod-wide event is a vertical stripe · each bar links to its broadcast</span>
          <span className={view.spacer} />
          <div className={view.legend}>
            <span>
              <i className={styles.legSpan} />
              live span
            </span>
            <span>
              <i className={styles.legBad} />
              bad
            </span>
            <span>
              <i className={styles.legWarn} />
              warn
            </span>
          </div>
        </div>
        <div className={view.panelB}>
          {timeline && timeline.rows.length > 0 ? (
            <FleetStripes timeline={timeline} />
          ) : (
            <p className={view.note}>
              {timeline
                ? timeline.coverage.reason === 'none'
                  ? 'This service has nothing stored yet — the range is unanswerable, not empty.'
                  : 'No broadcasts in this range.'
                : 'Loading…'}
            </p>
          )}
          {timeline?.rowsOmitted ? (
            <p className={view.note}>
              {timeline.rowsOmitted} further broadcast(s) are not drawn — beyond ~300 rows the stripe read stops working.
            </p>
          ) : null}
        </div>
      </section>

      <section className={view.panel} aria-label="Trends over the permanent rollups">
        <div className={view.panelH}>
          <h2 className={view.panelT}>Trends over the permanent rollups</h2>
          {bucket && <span className={view.note}>{bucket}</span>}
        </div>
        <div className={`${view.panelB} ${styles.trendBody}`}>
          <div className={view.toolbar}>
            <label className={view.control}>
              metric
              <select className={view.select} value={metric} onChange={(e) => setMetric(e.target.value)}>
                {METRICS.map((m) => (
                  <option key={m.value} value={m.value}>
                    {m.label}
                  </option>
                ))}
              </select>
            </label>
            <label className={view.control}>
              grouped by
              <select className={view.select} value={groupBy} onChange={(e) => setGroupBy(e.target.value)}>
                {GROUPS.map((g) => (
                  <option key={g.value} value={g.value}>
                    {g.label}
                  </option>
                ))}
              </select>
            </label>
            <div className={view.seg} role="group" aria-label="Statistic">
              {STATS.map((s) => (
                <button key={s} type="button" aria-pressed={stat === s} onClick={() => setStat(s)}>
                  {s}
                </button>
              ))}
            </div>
            <span className={view.spacer} />
            <span className={styles.thinKey}>
              <i aria-hidden />
              thin bucket, under 5 sessions
            </span>
          </div>

          {trend?.note && <p className={view.note}>{trend.note}</p>}
          <CoverageNote coverage={trend?.coverage} />
          <TimeChart
            series={trendSeries}
            bands={trendBands}
            fromMs={timeline?.fromMs}
            toMs={timeline?.toMs}
            height={240}
            showZoom={anyPoint && !allThin}
            empty={trend ? 'No rollups in this range.' : 'Loading…'}
            overlay={
              allThin ? (
                <>
                  Every bucket in this range holds fewer than 5 sessions, so no line is drawn — a baseline that thin
                  claims nothing. Thin buckets are shown hollow so the range is not mistaken for empty.
                  {groupBy ? (
                    <>
                      {' '}
                      Try <b>all together</b>
                      {range !== '90d' ? (
                        <>
                          {' '}
                          or a longer range
                        </>
                      ) : null}
                      .
                    </>
                  ) : range !== '90d' ? (
                    ' Try a longer range.'
                  ) : null}
                </>
              ) : undefined
            }
            ariaLabel={`fleet trend of ${metricLabel}`}
          />

          {groups.length >= 2 && (
            <div className={styles.compare}>
              <div className={view.toolbar}>
                <span className={view.eyebrow}>compare</span>
                <select className={view.select} value={cohortA} onChange={(e) => setCohortA(e.target.value)} aria-label="Cohort A">
                  <option value="">—</option>
                  {groups.map((g) => (
                    <option key={g} value={g}>
                      {g}
                    </option>
                  ))}
                </select>
                <span className={view.dimmer}>against</span>
                <select className={view.select} value={cohortB} onChange={(e) => setCohortB(e.target.value)} aria-label="Cohort B">
                  <option value="">—</option>
                  {groups.map((g) => (
                    <option key={g} value={g}>
                      {g}
                    </option>
                  ))}
                </select>
              </div>

              {cohort && (
                <div className={styles.cohort}>
                  <div className={styles.arm}>
                    <span className={view.dim}>
                      {cohort.a.label} · {stat} {metricLabel}
                    </span>
                    <b>{num(cohort.a.value, 1)}</b>
                    <span className={view.dimmer}>
                      {cohort.a.sessions} session{cohort.a.sessions === 1 ? '' : 's'}
                      {cohort.a.thin ? ' · thin' : ''}
                    </span>
                  </div>
                  <div className={`${styles.arm} ${styles.delta}`}>
                    <b>
                      {cohort.delta > 0 ? '+' : ''}
                      {num(cohort.delta, 1)}
                    </b>
                    {cohort.ratio ? <span className={view.dimmer}>{num((cohort.ratio - 1) * 100, 0)}%</span> : null}
                  </div>
                  <div className={styles.arm}>
                    <span className={view.dim}>
                      {cohort.b.label} · {stat} {metricLabel}
                    </span>
                    <b>{num(cohort.b.value, 1)}</b>
                    <span className={view.dimmer}>
                      {cohort.b.sessions} session{cohort.b.sessions === 1 ? '' : 's'}
                      {cohort.b.thin ? ' · thin' : ''}
                    </span>
                  </div>
                  {/* The sample size is stated whether or not it is flattering. A
                      cohort that hid a thin baseline would look like a conclusion. */}
                  {cohort.note && <p className={`${view.note} ${styles.cohortNote}`}>{cohort.note}</p>}
                  <CoverageNote coverage={cohort.coverage} />
                </div>
              )}
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

/**
 * The stripes themselves.
 *
 * Deliberately not a chart: each row is one broadcast's span with its degraded
 * sub-spans painted inside, positioned by percentage of the shared range. That
 * makes the vertical alignment exact and the DOM trivial — 300 rows of two
 * divs, rather than 300 ECharts instances.
 */
function FleetStripes({ timeline }: { timeline: FleetTimeline }) {
  const span = Math.max(1, timeline.toMs - timeline.fromMs);
  const pct = (ms: number) => ((ms - timeline.fromMs) / span) * 100;
  const ticks = useMemo(() => timeTicks(timeline.fromMs, timeline.toMs, 8), [timeline.fromMs, timeline.toMs]);
  const since = timeline.coverage.rollupsFromMs;
  const before = since && since > timeline.fromMs ? Math.min(100, pct(since)) : 0;

  return (
    <div className={styles.stripes}>
      <span />
      <div className={styles.axis} aria-hidden>
        {before > 0 && (
          <span
            className={styles.sinceLab}
            style={{ left: `${before}%` }}
            title="This service was installed after the start of this range: nothing before this is missing."
          >
            data since {shortDate(since)}
          </span>
        )}
        {ticks.map((t) => (
          <span key={t.atMs} className={styles.tickLab} style={{ left: `${t.pct}%` }}>
            {t.label}
          </span>
        ))}
      </div>
      <span />
      {timeline.rows.map((r) => (
        <StripeRow key={r.broadcastKey} row={r} pct={pct} ticks={ticks.map((t) => t.pct)} before={before} axisEnd={timeline.toMs} />
      ))}
    </div>
  );
}

function StripeRow({
  row: r,
  pct,
  ticks,
  before,
  axisEnd,
}: {
  row: FleetTimelineRow;
  pct: (ms: number) => number;
  ticks: number[];
  before: number;
  axisEnd: number;
}) {
  const left = Math.max(0, pct(r.fromMs));
  return (
    <>
      <a className={styles.key} href={href('broadcast', r.broadcastKey)} title={r.broadcastKey}>
        {shortId(r.broadcastKey, 12)}
        {r.live && (
          <span className={styles.live} title="still live">
            <i aria-hidden />
          </span>
        )}
        {r.rollupOnly && (
          <span className={view.tag} title="Raw samples pruned — rollup only">
            rollup
          </span>
        )}
      </a>
      <a className={styles.track} href={href('broadcast', r.broadcastKey)} aria-label={`broadcast ${r.broadcastKey}`}>
        {before > 0 && <span className={styles.notYet} style={{ width: `${before}%` }} />}
        {ticks.map((t) => (
          <span key={t} className={styles.gridline} style={{ left: `${t}%` }} />
        ))}
        <span className={styles.bar} style={{ left: `${left}%`, width: `${Math.max(0.4, pct(endOf(r, axisEnd)) - left)}%` }} />
        {(r.bands ?? []).map((b, i) =>
          b.severity === 'ok' ? null : (
            <span
              key={i}
              className={`${styles.band} ${styles[`band_${b.severity}`] ?? ''}`}
              title={`${b.severity} ${compactTime(b.fromMs)} → ${compactTime(b.toMs)}`}
              style={{ left: `${Math.max(0, pct(b.fromMs))}%`, width: `${Math.max(0.3, pct(b.toMs) - pct(b.fromMs))}%` }}
            />
          ),
        )}
      </a>
      <span className={styles.meta}>
        {r.viewers} viewer{r.viewers === 1 ? '' : 's'}
      </span>
    </>
  );
}

/**
 * A broadcast that is still live has no end. The axis's own end stands in for
 * "now" so its bar reaches the right edge — which is exactly what "still
 * going" looks like, and is why the fallback lives here rather than in the
 * server's projection, where a stored `toMs` of 0 is the honest value.
 */
function endOf(row: FleetTimelineRow, axisEndMs: number): number {
  return row.toMs > 0 ? row.toMs : axisEndMs;
}
