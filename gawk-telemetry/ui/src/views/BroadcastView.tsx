import { useEffect, useMemo, useState } from 'react';

import { fetchBroadcast, fetchBroadcastDiagnose } from '../api/client.ts';
import type { BroadcastDetail, Lane, Report } from '../api/types.ts';
import { TimeChart, type Band, type Marker } from '../charts/TimeChart.tsx';
import { severityBand } from '../charts/echarts.ts';
import { AnnotationPanel } from '../components/Annotations.tsx';
import { useAnnotations } from '../lib/useAnnotations.ts';
import { CoverageNote, DesktopOnly } from '../components/Chrome.tsx';
import { ReportPanel } from '../components/Findings.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { absoluteTime, dur, EMPTY, rangeLabel, shortId, timeZoneLabel } from '../lib/format.ts';
import { href, isBroadcastKey, useDocumentTitle } from '../router/router.ts';
import { useLiveStore } from '../state/liveStore.ts';
import styles from './BroadcastView.module.css';
import view from './view.module.css';

// TH4: **the isolating surface** — Q3 and Q4.
//
// One broadcast, one absolute time axis, one lane per participant: broadcaster
// on top, each viewer below, plus a relay lane. The lanes are separate ECharts
// instances joined by `echarts.connect()`, so one crosshair and one zoom govern
// all of them (UD11) — which IS the feature, not a nicety: *"did they all dip
// at the same second, or just that one viewer?"* is a question about vertical
// alignment, and no amount of per-session graphs answers it.
//
// The honesty that makes a shared axis legitimate is carried per lane and
// stated on screen: each source has its own cadence (client ~2 s, relay ~5 s)
// and each client has its own clock, measured and applied rather than assumed.

const REFRESH_MS = 5000;

export function BroadcastView({ broadcastKey }: { broadcastKey: string }) {
  const [detail, setDetail] = useState<BroadcastDetail | null>(null);
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const paused = useLiveStore((s) => s.paused);
  const { notes, reload: reloadNotes } = useAnnotations({ broadcastKey });

  useDocumentTitle(`broadcast ${shortId(broadcastKey)} — gawk telemetry`);

  useEffect(() => {
    if (!isBroadcastKey(broadcastKey)) {
      setError('not a broadcast key');
      setLoading(false);
      return;
    }
    const ac = new AbortController();
    const load = () =>
      fetchBroadcast(broadcastKey, { sinceMs: Date.now() - 7 * 24 * 3600_000 }, ac.signal)
        .then((d) => {
          setDetail(d);
          setError(null);
        })
        .catch((e: unknown) => {
          if (ac.signal.aborted) return;
          setError(e instanceof Error ? e.message : String(e));
        })
        .finally(() => setLoading(false));
    void load();
    fetchBroadcastDiagnose(broadcastKey, ac.signal).then(setReport).catch(() => setReport(null));
    return () => ac.abort();
  }, [broadcastKey]);

  useEffect(() => {
    if (!detail?.live || paused) return;
    const id = setInterval(() => {
      void fetchBroadcast(broadcastKey, { sinceMs: Date.now() - 7 * 24 * 3600_000 })
        .then(setDetail)
        .catch(() => undefined);
    }, REFRESH_MS);
    return () => clearInterval(id);
  }, [detail?.live, paused, broadcastKey]);

  const noteMarkers = useMemo<Marker[]>(
    () => notes.filter((n) => n.atMs).map((n) => ({ atMs: n.atMs!, label: `note: ${n.text.slice(0, 24)}`, color: '#58a6ff' })),
    [notes],
  );

  if (error) {
    return (
      <div className={view.page}>
        <p className={view.error}>
          {error === 'not a broadcast key'
            ? `“${broadcastKey}” is not a broadcast key (12 hex characters).`
            : error}
        </p>
      </div>
    );
  }
  if (loading || !detail) return <p className={view.loading}>Assembling the lanes…</p>;

  const group = `broadcast-${broadcastKey}`;
  const rehomeMarkers: Marker[] = (detail.rehomes ?? []).map((r) => ({
    atMs: r.atMs,
    label: `re-home → ${r.toPod}${r.toRole ? ` (${r.toRole})` : ''}`,
    color: '#a371f7',
  }));

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Broadcast {shortId(broadcastKey)}</h1>
        <span className={view.subtitle}>
          {detail.live ? 'LIVE · ' : ''}
          {rangeLabel(detail.fromMs, detail.toMs)} {timeZoneLabel(detail.fromMs)}
        </span>
        <span className={view.spacer} />
        <a className={view.button} href={href('history', undefined, { broadcast: broadcastKey })}>
          sessions in history →
        </a>
      </header>

      <CoverageNote coverage={detail.coverage} />
      <DesktopOnly what="The multi-lane broadcast timeline" />

      {detail.lanesOmitted ? (
        <p className={view.rollupOnly}>
          {detail.lanesOmitted} further participant(s) are not drawn — above ~24 lanes the vertical
          read stops working. Use History filtered to this broadcast to reach them.
        </p>
      ) : null}

      {report && (
        <section>
          <h2 className={view.panelTitle}>Broadcast verdict</h2>
          <ReportPanel report={report} title={`broadcast ${broadcastKey}`} />
        </section>
      )}

      <section className={styles.lanes}>
        {detail.lanes.map((lane) => (
          <LaneRow
            key={lane.sessionId ?? lane.kind}
            lane={lane}
            fromMs={detail.fromMs}
            toMs={detail.toMs}
            group={group}
            markers={lane.kind === 'relay' ? [...rehomeMarkers, ...noteMarkers] : noteMarkers}
          />
        ))}
      </section>

      <p className={view.note}>
        One crosshair and one zoom govern every lane. Each lane is drawn at its own source’s
        cadence and never finer — a client samples every ~2 s and the relay is scraped every ~5 s,
        so an alignment tighter than that is not something this data can support.
      </p>

      <AnnotationPanel
        broadcastKey={broadcastKey}
        atMs={detail.toMs}
        notes={notes}
        onChange={reloadNotes}
      />
    </div>
  );
}

function LaneRow({
  lane,
  fromMs,
  toMs,
  group,
  markers,
}: {
  lane: Lane;
  fromMs: number;
  toMs: number;
  group: string;
  markers: Marker[];
}) {
  const bands: Band[] = [];
  // The SPAN, drawn as the region outside which this participant did not
  // exist. A viewer who joined late must read as "was not here", never as a
  // gap in a line.
  if (lane.startedAtMs > fromMs) {
    bands.push({ fromMs, toMs: lane.startedAtMs, label: 'not joined', color: 'rgba(11,12,14,0.55)' });
  }
  if (lane.endedAtMs && lane.endedAtMs < toMs) {
    bands.push({ fromMs: lane.endedAtMs, toMs, label: 'left', color: 'rgba(11,12,14,0.55)' });
  }
  for (const h of lane.hidden ?? []) {
    bands.push({ fromMs: h.fromMs, toMs: h.toMs, label: 'tab hidden' });
  }
  for (const d of lane.dips ?? []) {
    bands.push({ fromMs: d.fromMs, toMs: d.toMs, label: 'dip', color: severityBand('bad') });
  }

  const laneMarkers: Marker[] = [
    ...markers,
    ...(lane.events ?? []).map((e) => ({ atMs: e.atMs, label: e.kind })),
  ];

  return (
    <div className={styles.lane}>
      <div className={styles.laneHead}>
        <SeverityBadge severity={lane.severity} />
        <span className={styles.laneKind}>{lane.kind}</span>
        {lane.sessionId ? (
          <a className={styles.laneId} href={href('session', lane.sessionId)}>
            {shortId(lane.sessionId)}
          </a>
        ) : (
          <span className={styles.laneId}>relay</span>
        )}
        <span className={styles.laneMeta}>
          {[lane.browser, lane.os, lane.deliveryMode].filter(Boolean).join(' / ') || EMPTY}
        </span>
        <span className={view.spacer} />
        {/* UD9 as a visible claim: no lane may be read at a resolution its
            source cannot support, so every lane states what its source is. */}
        <span className={styles.cadence} title="This source's own reporting interval">
          every {dur(lane.cadenceMs)}
        </span>
        {lane.clockOffsetMs ? (
          <span className={styles.cadence} title="Measured against the service's receive clock and applied to this lane">
            clock {lane.clockOffsetMs > 0 ? '−' : '+'}
            {dur(Math.abs(lane.clockOffsetMs))}
          </span>
        ) : null}
        <span className={styles.laneSpan}>
          {absoluteTime(lane.startedAtMs)}
          {lane.endedAtMs ? ` → ${absoluteTime(lane.endedAtMs)}` : ' → still here'}
        </span>
      </div>

      {lane.rollupOnly ? (
        <p className={styles.laneNote}>{lane.note}</p>
      ) : (
        <TimeChart
          series={[
            {
              key: lane.primary ?? 'value',
              label: lane.primary ?? 'rate',
              data: (lane.points ?? []).map((p) => [p.atMs, p.value] as [number, number | null]),
              unit: lane.unit,
              area: true,
            },
          ]}
          bands={bands}
          markers={laneMarkers}
          fromMs={fromMs}
          toMs={toMs}
          height={96}
          group={group}
          unit={lane.unit}
          note={lane.note}
          empty={lane.kind === 'relay' ? 'No relay observation of this broadcast.' : 'No samples.'}
          ariaLabel={`${lane.kind} ${lane.sessionId ?? ''} rate over the broadcast`}
        />
      )}
      {lane.verdict && <p className={styles.laneVerdict}>{lane.verdict}</p>}
    </div>
  );
}
