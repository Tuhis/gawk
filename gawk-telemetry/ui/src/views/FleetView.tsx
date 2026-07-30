import { useEffect, useMemo, useState } from 'react';

import { fetchCohorts, fetchFleetTimeline, fetchTrends } from '../api/client.ts';
import type { Cohort, FleetTimeline, FleetTimelineRow, Trend } from '../api/types.ts';
import { TimeChart, type Series } from '../charts/TimeChart.tsx';
import { severityBand } from '../charts/echarts.ts';
import { CoverageNote, DesktopOnly } from '../components/Chrome.tsx';
import { absoluteTime, num, shortId, timeZoneLabel } from '../lib/format.ts';
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
// The view says when it is answering from rollups alone.
//
// Grafana was deferred again (owner, 2026-07-29), so this is the only home for
// a trend question.

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

  const trendSeries = useMemo<Series[]>(
    () =>
      (trend?.series ?? []).map((s) => ({
        key: s.group,
        label: `${s.group} (${s.sessions})`,
        // A thin bucket contributes a `null`, so the line BREAKS rather than
        // dipping through a number computed from two sessions. `FleetSummary`
        // already refuses to over-claim below five; that honesty carries here
        // instead of being re-decided per view.
        data: s.points.map((p) => [p.atMs, p.thin ? null : p.value] as [number, number | null]),
      })),
    [trend],
  );

  const groups = useMemo(() => (trend?.series ?? []).map((s) => s.group), [trend]);

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Fleet</h1>
        <span className={view.subtitle}>times in {timeZoneLabel()}</span>
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

      {error && <p className={view.error}>{error}</p>}
      <CoverageNote coverage={timeline?.coverage} />
      <DesktopOnly what="The fleet timeline" />

      <section className={view.panel}>
        <h2 className={view.panelTitle}>
          Broadcasts on one axis — a pod-wide event is a vertical stripe
        </h2>
        {timeline && timeline.rows.length > 0 ? (
          <FleetStripes timeline={timeline} />
        ) : (
          <p className={view.rollupOnly}>
            No broadcasts in this range.
            {timeline?.coverage.note ? ' See the note above.' : ''}
          </p>
        )}
        {timeline?.rowsOmitted ? (
          <p className={view.note}>
            {timeline.rowsOmitted} further broadcast(s) are not drawn — beyond ~300 rows the stripe
            read stops working.
          </p>
        ) : null}
      </section>

      <section className={view.panel}>
        <h2 className={view.panelTitle}>Trends over the permanent rollups</h2>
        <div className={view.controls}>
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
          <label className={view.control}>
            stat
            <select className={view.select} value={stat} onChange={(e) => setStat(e.target.value)}>
              <option value="median">median</option>
              <option value="p95">p95</option>
              <option value="mean">mean</option>
            </select>
          </label>
        </div>

        {trend?.note && <p className={view.note}>{trend.note}</p>}
        <TimeChart
          series={trendSeries}
          height={220}
          showZoom
          note="A bucket computed from fewer than 5 sessions is drawn as a break, not as a point — the baseline is too thin to claim anything."
          empty="No rollups in this range."
          ariaLabel="fleet trend"
        />

        {groups.length >= 2 && (
          <div className={view.controls}>
            <label className={view.control}>
              compare
              <select className={view.select} value={cohortA} onChange={(e) => setCohortA(e.target.value)}>
                <option value="">—</option>
                {groups.map((g) => (
                  <option key={g} value={g}>
                    {g}
                  </option>
                ))}
              </select>
            </label>
            <label className={view.control}>
              against
              <select className={view.select} value={cohortB} onChange={(e) => setCohortB(e.target.value)}>
                <option value="">—</option>
                {groups.map((g) => (
                  <option key={g} value={g}>
                    {g}
                  </option>
                ))}
              </select>
            </label>
          </div>
        )}

        {cohort && (
          <div className={styles.cohort}>
            <div className={styles.arm}>
              <span className={styles.armLabel}>{cohort.a.label}</span>
              <span className={`${styles.armValue} tnum`}>{num(cohort.a.value, 1)}</span>
              <span className={styles.armN}>{cohort.a.sessions} sessions</span>
            </div>
            <div className={styles.armDelta}>
              <span className="tnum">
                {cohort.delta > 0 ? '+' : ''}
                {num(cohort.delta, 1)}
              </span>
              {cohort.ratio ? <span className={view.dim}> ({num(cohort.ratio * 100, 0)}%)</span> : null}
            </div>
            <div className={styles.arm}>
              <span className={styles.armLabel}>{cohort.b.label}</span>
              <span className={`${styles.armValue} tnum`}>{num(cohort.b.value, 1)}</span>
              <span className={styles.armN}>{cohort.b.sessions} sessions</span>
            </div>
            {/* The sample size is stated whether or not it is flattering. A
                cohort that hid a thin baseline would look like a conclusion. */}
            {cohort.note && <p className={styles.cohortNote}>{cohort.note}</p>}
          </div>
        )}
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

  return (
    <div className={styles.stripes}>
      <div className={styles.axis}>
        <span>{absoluteTime(timeline.fromMs)}</span>
        <span className={view.spacer} />
        <span>{absoluteTime(timeline.toMs)}</span>
      </div>
      {timeline.rows.map((r) => (
        <a key={r.broadcastKey} className={styles.stripeRow} href={href('broadcast', r.broadcastKey)}>
          <span className={styles.stripeKey}>
            {shortId(r.broadcastKey)}
            {r.live && <span className={styles.liveDot}>●</span>}
            {r.rollupOnly && <span className={styles.tag}>R</span>}
          </span>
          <span className={styles.track}>
            <span
              className={styles.span}
              style={{
                left: `${Math.max(0, pct(r.fromMs))}%`,
                width: `${Math.max(0.4, pct(endOf(r, timeline.toMs)) - pct(r.fromMs))}%`,
              }}
            />
            {(r.bands ?? []).map((b, i) => (
              <span
                key={i}
                className={styles.band}
                title={`${b.severity} ${absoluteTime(b.fromMs)}`}
                style={{
                  left: `${Math.max(0, pct(b.fromMs))}%`,
                  width: `${Math.max(0.3, pct(b.toMs) - pct(b.fromMs))}%`,
                  background: severityBand(b.severity),
                }}
              />
            ))}
          </span>
          <span className={styles.stripeMeta}>
            {r.viewers} viewer{r.viewers === 1 ? '' : 's'}
          </span>
        </a>
      ))}
    </div>
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
