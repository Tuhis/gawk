import { useEffect, useMemo, useState } from 'react';

import { fetchBroadcast, fetchBroadcastDiagnose } from '../api/client.ts';
import type { BroadcastDetail, Lane, LegLane, Report, Severity } from '../api/types.ts';
import { lastValue, niceMax, readoutValue, stepOf, useStack, valueAt, type Stack } from '../charts/stack.ts';
import { usePalette } from '../charts/palette.ts';
import { StackChart, type StackBand, type StackMark } from '../charts/StackChart.tsx';
import { TimeAxis, ZoomBar } from '../charts/TimeAxis.tsx';
import { AnnotationPanel } from '../components/Annotations.tsx';
import { CoverageNote, DesktopOnly } from '../components/Chrome.tsx';
import { ReportPanel } from '../components/Findings.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { clockTime, compactTime, dur, shortId } from '../lib/format.ts';
import { rank } from '../lib/severity.ts';
import { useAnnotations } from '../lib/useAnnotations.ts';
import { href, isBroadcastKey, useDocumentTitle } from '../router/router.ts';
import { useLiveStore } from '../state/liveStore.ts';
import { usePinFollow } from '../state/usePinFollow.ts';
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
//
// R36 (docs/86 TU3): the stack reads through ONE sticky time axis and a
// readout rail — each lane's value at the crosshair, in a fixed column at its
// right edge — instead of a tooltip per lane and an axis per lane. Nothing is
// written inside a lane; an event is a tick on its floor and its name is in
// the rail. A striped viewer's legs hang under it as sub-lanes (TU5), and a
// viewer served by a PoP says so (TU6).

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

  return <BroadcastPage broadcastKey={broadcastKey} detail={detail} report={report} notes={notes} reloadNotes={reloadNotes} />;
}

function BroadcastPage({
  broadcastKey,
  detail,
  report,
  notes,
  reloadNotes,
}: {
  broadcastKey: string;
  detail: BroadcastDetail;
  report: Report | null;
  notes: ReturnType<typeof useAnnotations>['notes'];
  reloadNotes: () => void;
}) {
  const stack = useStack(detail.fromMs, detail.toMs);
  const group = `broadcast-${broadcastKey}`;
  usePinFollow(stack.cursorMs);

  const noteMarks = useMemo<StackMark[]>(
    () => notes.filter((n) => n.atMs).map((n) => ({ atMs: n.atMs!, kind: 'note', label: `note: ${n.text}` })),
    [notes],
  );
  const rehomeMarks = useMemo<StackMark[]>(
    () =>
      (detail.rehomes ?? []).map((r) => ({
        atMs: r.atMs,
        kind: 'rehome',
        label: `re-homed → ${r.toPod}${r.toRole ? ` (${r.toRole})` : ''}`,
      })),
    [detail.rehomes],
  );

  const worst = detail.lanes.reduce<Severity>((acc, l) => (rank(l.severity) > rank(acc) ? l.severity : acc), 'unknown');
  const viewers = detail.lanes.filter((l) => l.kind === 'viewer').length;
  const anyLegs = detail.lanes.some((l) => l.legs?.length);

  return (
    <div className={view.page}>
      <div className={view.crumbs}>
        <a href={href('live')}>Live</a>
        <span>/</span>
        <span>broadcast</span>
      </div>
      <header className={view.head}>
        <SeverityBadge severity={report?.findings?.[0]?.severity ?? worst} />
        <h1 className={view.title}>
          Broadcast <span className={view.titleId}>{broadcastKey}</span>
        </h1>
        {detail.live && (
          <span className={styles.live}>
            <i aria-hidden />
            LIVE
          </span>
        )}
        <span className={view.subtitle}>
          {clockTime(detail.fromMs)} → {detail.live ? 'now' : clockTime(detail.toMs)} · {dur(detail.toMs - detail.fromMs)} ·{' '}
          {viewers} viewer{viewers === 1 ? '' : 's'}
        </span>
        <span className={view.spacer} />
        <button
          type="button"
          className={view.button}
          onClick={() => {
            const el = document.getElementById('broadcast-note');
            el?.scrollIntoView({ block: 'center' });
            el?.focus();
          }}
        >
          add note
        </button>
        <a className={view.button} href={href('history', undefined, { broadcast: broadcastKey })}>
          sessions in history →
        </a>
      </header>

      <CoverageNote coverage={detail.coverage} />
      <DesktopOnly what="The multi-lane broadcast timeline" />

      {report && <ReportPanel report={report} heading="Broadcast verdict" title={`broadcast ${broadcastKey}`} />}

      <section className={view.panel} aria-label="Every participant on one axis">
        <div className={view.panelH}>
          <h2 className={view.panelT}>Every participant on one axis</h2>
          <span className={view.note}>one crosshair, one zoom · each lane at its own source’s cadence</span>
          <span className={view.spacer} />
          <Legend legs={anyLegs} />
        </div>

        {detail.lanesOmitted ? (
          <p className={`${view.note} ${styles.omitted}`}>
            {detail.lanesOmitted} further participant{detail.lanesOmitted === 1 ? ' is' : 's are'} not drawn — above
            ~24 lanes the vertical read stops working. History, filtered to this broadcast, has all of them.
          </p>
        ) : null}

        <div className={styles.stack}>
          <div className={styles.axisRow}>
            <div className={styles.lab}>
              <span className={view.eyebrow}>participant</span>
            </div>
            <TimeAxis viewFrom={stack.viewFrom} viewTo={stack.viewTo} cursorMs={stack.cursorMs} />
            <div className={styles.ro}>
              <span className={view.eyebrow}>{stack.cursorMs === null ? 'latest' : 'at crosshair'}</span>
            </div>
          </div>

          {detail.lanes.map((lane) => (
            <LaneRows
              key={lane.sessionId ?? lane.kind}
              lane={lane}
              detail={detail}
              group={group}
              stack={stack}
              marks={lane.kind === 'relay' ? [...rehomeMarks, ...noteMarks] : noteMarks}
            />
          ))}

          <div className={styles.zoomRow}>
            <span />
            <ZoomBar fromMs={detail.fromMs} toMs={detail.toMs} group={group} onInit={stack.register} />
            <span />
          </div>
        </div>
        <p className={`${view.note} ${styles.foot}`}>
          Client lanes sample every ~2 s and the relay is scraped every ~5 s; nothing here is aligned tighter than
          that. The axis starts when the broadcast did — there is no range before it to explain. Drag across a lane,
          or the bar below, to zoom every lane at once.
        </p>
      </section>

      <AnnotationPanel
        broadcastKey={broadcastKey}
        atMs={detail.toMs}
        followPin
        notes={notes}
        onChange={reloadNotes}
        inputId="broadcast-note"
      />
    </div>
  );
}

function Legend({ legs }: { legs: boolean }) {
  return (
    <div className={view.legend} aria-label="Legend">
      <span>
        <i style={{ background: 'var(--s1)' }} />
        rate
      </span>
      <span>
        <i className={styles.legDip} />
        dip
      </span>
      <span>
        <i className={styles.legVeil} />
        tab hidden / not joined
      </span>
      <span>
        <i className={styles.legEvent} />
        event
      </span>
      <span>
        <i className={styles.legRehome} />
        re-home
      </span>
      <span>
        <i className={styles.legNote} />
        note
      </span>
      {legs && (
        <span>
          <i style={{ background: 'var(--s5)' }} />
          stripe leg
        </span>
      )}
    </div>
  );
}

type Pt = [number, number | null];

/** A lane and, under it, its stripe legs. */
function LaneRows({
  lane,
  detail,
  group,
  stack,
  marks,
}: {
  lane: Lane;
  detail: BroadcastDetail;
  group: string;
  stack: Stack;
  marks: StackMark[];
}) {
  const { fromMs, toMs } = detail;
  const data = useMemo<Pt[]>(() => (lane.points ?? []).map((p) => [p.atMs, p.value]), [lane.points]);

  const bands = useMemo<StackBand[]>(() => {
    const out: StackBand[] = [];
    // The SPAN, drawn as the region outside which this participant did not
    // exist. A viewer who joined late must read as "was not here", never as a
    // gap in a line.
    if (lane.startedAtMs > fromMs) out.push({ fromMs, toMs: lane.startedAtMs, kind: 'veil', label: 'not joined' });
    if (lane.endedAtMs && lane.endedAtMs < toMs) out.push({ fromMs: lane.endedAtMs, toMs, kind: 'veil', label: 'left' });
    for (const h of lane.hidden ?? []) out.push({ fromMs: h.fromMs, toMs: h.toMs, kind: 'veil', label: 'tab hidden' });
    for (const d of lane.dips ?? []) out.push({ fromMs: d.fromMs, toMs: d.toMs, kind: 'dip', label: 'dip' });
    return out;
  }, [lane, fromMs, toMs]);

  const ticks = useMemo(() => (lane.events ?? []).map((e) => e.atMs), [lane.events]);
  const step = useMemo(() => stepOf(data), [data]);
  const scale = useMemo(() => niceMax(data.reduce((m, [, v]) => (v !== null && v > m ? v : m), 0) * 1.08), [data]);

  const t = stack.cursorMs;
  // A reading older than twice the source's cadence — or, on a downsampled
  // lane, one and a half of its point spacing — no longer describes the
  // crosshair's instant: it is absent there (UD9), not carried forward.
  const maxAge = Math.max(lane.cadenceMs * 2, step * 1.5, 4000);
  const value = t === null ? lastValue(data) : valueAt(data, t, maxAge);
  const near = t === null ? undefined : nearestEvent(lane, t, Math.max(lane.cadenceMs, 2000));
  const at = t === null ? undefined : whereIs(lane, bands, t);

  const tags: Array<{ text: string; title?: string; accent?: boolean }> = [];
  const client = [lane.browser, lane.os].filter(Boolean).join(' / ');
  if (client) tags.push({ text: client });
  if (lane.deliveryMode) tags.push({ text: lane.deliveryMode });
  if (lane.appVersion) tags.push({ text: lane.appVersion });
  if (lane.pop) {
    tags.push({
      text: `PoP ${lane.pop}`,
      accent: true,
      title: 'Served by a point of presence: its relay-side figures are on the PoP, not in this broadcast’s relay lane.',
    });
  }
  if (lane.legs?.length) {
    const pods = new Set(lane.legs.map((l) => l.pod)).size;
    tags.push({
      text: `striped ×${lane.legs.length}${pods > 1 ? ` · ${pods} pods` : ''}`,
      accent: true,
      title: `One viewer carried on 1 + ${lane.legs.length} relay subscribers: its deltas ride the legs below.`,
    });
  }
  tags.push({ text: `every ${dur(lane.cadenceMs)}`, title: 'This source’s own reporting interval: no lane is drawn finer (UD9).' });
  if (lane.clockOffsetMs) {
    tags.push({
      text: `clock ${lane.clockOffsetMs > 0 ? '−' : '+'}${dur(Math.abs(lane.clockOffsetMs))}`,
      title: 'Measured against the service’s receive clock and applied to this lane',
    });
  }

  const emptyText = lane.rollupOnly
    ? lane.note || 'Raw samples pruned — the rollup and its verdict remain.'
    : lane.kind === 'relay'
      ? 'No relay observation of this broadcast.'
      : lane.pop
        ? 'No client samples; its relay figures are on the PoP.'
        : 'Never reported — the relay carries it; the client has not said anything.';

  return (
    <>
      <div className={styles.lrow}>
        <div className={styles.lab}>
          <div className={styles.l1}>
            <SeverityBadge severity={lane.severity} />
            <span className={styles.kind}>{lane.kind}</span>
            {lane.sessionId ? (
              <a className={styles.id} href={href('session', lane.sessionId)} title={lane.sessionId}>
                {shortId(lane.sessionId)}
              </a>
            ) : null}
          </div>
          <div className={styles.l2}>
            {tags.map((tg) => (
              <span key={tg.text} className={`${view.tag} ${tg.accent ? view.tagAccent : ''}`} title={tg.title}>
                {tg.text}
              </span>
            ))}
          </div>
          <div className={styles.l3} title={lane.verdict}>
            {compactTime(lane.startedAtMs)}
            {lane.endedAtMs ? ` → ${clockTime(lane.endedAtMs)}` : ' → still here'}
            {lane.verdict ? ` · ${lane.verdict}` : ''}
          </div>
        </div>
        <StackChart
          series={[{ key: lane.primary ?? 'value', label: lane.primary ?? 'rate', data, area: true }]}
          bands={bands}
          marks={marks}
          ticks={ticks}
          fromMs={fromMs}
          toMs={toMs}
          height={96}
          group={group}
          onInit={stack.register}
          empty={lane.rollupOnly || data.length === 0 ? emptyText : undefined}
          ariaLabel={`${lane.kind} ${lane.sessionId ?? ''} ${lane.primary ?? 'rate'} over the broadcast`}
        />
        <div className={styles.ro}>
          <span className={styles.v}>{readoutValue(value)}</span>
          <span className={styles.u}>
            {lane.unit ?? lane.primary ?? ''}
            {data.length ? ` · 0–${readoutValue(scale)}` : ''}
          </span>
          {near ? (
            <span className={styles.evt} title={near.detail}>
              ▏{near.kind}
            </span>
          ) : at ? (
            <span className={styles.evt}>{at}</span>
          ) : null}
        </div>
      </div>
      {(lane.legs ?? []).map((leg, i) => (
        <LegRow key={`${leg.member}-${leg.pod}-${i}`} leg={leg} fromMs={fromMs} toMs={toMs} group={group} stack={stack} marks={marks} />
      ))}
      {lane.legsOmitted ? (
        <div className={`${styles.lrow} ${styles.sub}`}>
          <div className={styles.lab}>
            <span className={styles.legName}>+{lane.legsOmitted} older leg sessions not drawn</span>
          </div>
          <span />
          <span />
        </div>
      ) : null}
    </>
  );
}

function LegRow({
  leg,
  fromMs,
  toMs,
  group,
  stack,
  marks,
}: {
  leg: LegLane;
  fromMs: number;
  toMs: number;
  group: string;
  stack: Stack;
  marks: StackMark[];
}) {
  const data = useMemo<Pt[]>(() => (leg.points ?? []).map((p) => [p.atMs, p.value]), [leg.points]);
  const t = stack.cursorMs;
  const value = t === null ? lastValue(data) : valueAt(data, t, Math.max(15_000, stepOf(data) * 1.5));
  const p = usePalette();
  // A canvas cannot read a CSS variable: the slate series colour comes from
  // the palette, which read it from the same token.
  const series = useMemo(() => [{ key: 'leg', label: 'leg', data, color: p.series[4] }], [data, p]);
  return (
    <div className={`${styles.lrow} ${styles.sub}`}>
      <div className={styles.lab}>
        <span className={styles.legName}>
          <span className={styles.legLine} aria-hidden />
          leg {leg.member + 1} / {leg.n}
        </span>
        <span className={styles.legPod} title={leg.role ? `${leg.pod} (${leg.role})` : leg.pod}>
          {leg.pod}
          {leg.role ? ` · ${leg.role}` : ''}
        </span>
      </div>
      <StackChart
        series={series}
        marks={marks.filter((m) => m.kind === 'note')}
        fromMs={fromMs}
        toMs={toMs}
        height={34}
        group={group}
        onInit={stack.register}
        quiet
        empty={data.length === 0 ? 'no stored series for this leg' : undefined}
        ariaLabel={`stripe leg ${leg.member + 1} of ${leg.n}`}
      />
      <div className={styles.ro}>
        <span className={styles.vSub}>{readoutValue(value)}</span>
        <span className={styles.u}>
          {leg.unit ?? 'dropped datagrams/s'}
          {leg.datagramsDropped !== undefined ? ` · ${leg.datagramsDropped.toLocaleString()} in all` : ''}
        </span>
      </div>
    </div>
  );
}

/** The stored event nearest the crosshair, within one cadence — named in the rail. */
function nearestEvent(lane: Lane, t: number, within: number) {
  let best: { kind: string; detail?: string; d: number } | undefined;
  for (const e of lane.events ?? []) {
    const d = Math.abs(e.atMs - t);
    if (d <= within && (!best || d < best.d)) best = { kind: e.kind, detail: e.detail, d };
  }
  return best;
}

/** Which band the crosshair sits in, so a veiled stretch says why it is empty. */
function whereIs(lane: Lane, bands: StackBand[], t: number): string | undefined {
  for (const b of bands) if (t >= b.fromMs && t <= b.toMs) return b.label;
  if (t < lane.startedAtMs) return 'not joined';
  return undefined;
}

