import type { ContentFlag, Timeline, TimelineBucket } from '../api/types.ts';
import { formatScore } from '../lib/flags.ts';
import { formatInstant } from '../lib/format.ts';
import { axisPct, axisTicks, incidentEnd, shortLabel, thresholdAt } from '../lib/timeline.ts';
import styles from '../views/BroadcastTimelineView.module.css';

/**
 * The timeline's tracks on one time axis (R70, docs/72 OD1): publisher
 * sessions, a bar per bucket (the highest score among the visible labels), a
 * strip per label, the incidents, and the moderation events.
 *
 * Every level is read from the timeline, never assumed (OD6): the floor and
 * each session's threshold are the sampler's, the critical level is the
 * deployment's. A session that reported no threshold draws none.
 */
export function TimelineChart({
  timeline,
  hidden,
  onToggleLabel,
  selected,
  onSelect,
}: {
  timeline: Timeline;
  hidden: ReadonlySet<string>;
  onToggleLabel: (label: string) => void;
  selected: number | null;
  onSelect: (flag: ContentFlag) => void;
}) {
  const from = Date.parse(timeline.from);
  const to = Date.parse(timeline.to);
  const bucketMs = timeline.bucketSeconds * 1000;
  const n = Math.max(1, Math.ceil((to - from) / bucketMs));
  const x = (t: number) => ((t - from) / (to - from)) * n;
  const { floor, critical } = timeline.levels;
  const visible = timeline.labels.filter((l) => !hidden.has(l));
  const scoreOf = (b: TimelineBucket) => Math.max(0, ...visible.map((l) => b.scores[l] ?? 0));
  const ticks = axisTicks(from, to);

  const marks = [
    ...timeline.events.map((e) => ({ at: Date.parse(e.occurredAt), title: `${formatInstant(e.occurredAt)} · ${e.summary || e.type}`, ...eventGlyph(e.type) })),
    ...timeline.incidents
      .filter((f) => f.acknowledgedAt)
      .map((f) => ({
        at: Date.parse(f.acknowledgedAt as string),
        title: `${formatInstant(f.acknowledgedAt as string)} · flag #${f.id} acknowledged${f.acknowledgedBy ? ` by ${f.acknowledgedBy}` : ''}`,
        glyph: '✓',
        tone: styles.markAck,
      })),
  ].filter((m) => Number.isFinite(m.at) && m.at >= from && m.at <= to);

  return (
    <div className={styles.tracks}>
      <span className={styles.trackLabel}>Session</span>
      <div className={styles.sessionTrack}>
        {timeline.sessions.map((s) => {
          const left = axisPct(Date.parse(s.firstSampleAt), from, to);
          const right = axisPct(Date.parse(s.lastSampleAt) + bucketMs, from, to);
          return (
            <span
              key={s.publisherEpoch}
              className={styles.session}
              style={{ left: `${left}%`, width: `${Math.max(0.5, right - left)}%` }}
              title={`Publisher session ${s.publisherEpoch} · ${s.mode} mode · ${s.samplesTaken} samples · ${s.samplerId}`}
            >
              {s.mode === 'observe' ? 'observe' : 'flag'} · {s.samplesTaken} samples
            </span>
          );
        })}
      </div>

      <span className={styles.trackLabel}>
        Score
        <span className={styles.trackHint}>
          {timeline.levels.threshold != null ? `threshold ${formatScore(timeline.levels.threshold)}` : 'threshold unknown'}
          <br />
          critical {formatScore(critical)} · floor {formatScore(floor)}
        </span>
      </span>
      <div className={styles.chart}>
        <svg
          role="img"
          aria-label={`Highest flagged-label score per ${timeline.bucketSeconds} seconds`}
          viewBox={`0 0 ${n} 100`}
          preserveAspectRatio="none"
          className={styles.chartSvg}
        >
          {timeline.sessions.map((s) => (
            <rect
              key={`span-${s.publisherEpoch}`}
              x={x(Date.parse(s.firstSampleAt))}
              y={0}
              width={Math.max(0.5, x(Date.parse(s.lastSampleAt) + bucketMs) - x(Date.parse(s.firstSampleAt)))}
              height={100}
              className={styles.sampledSpan}
            />
          ))}
          {timeline.incidents.map((f) => (
            <rect
              key={`band-${f.id}`}
              x={x(Date.parse(f.raisedAt))}
              y={0}
              width={Math.max(0.5, x(incidentEnd(f, to)) - x(Date.parse(f.raisedAt)))}
              height={100}
              className={f.id === selected ? styles.bandSelected : styles.band}
            />
          ))}
          <rect x={0} y={100 - floor * 100} width={n} height={floor * 100} className={styles.belowFloor} />
          {timeline.buckets.map((b) => {
            const t = Date.parse(b.start);
            const v = scoreOf(b);
            if (v < floor) return null;
            const th = thresholdAt(timeline, t);
            const tone = v >= critical ? styles.barCritical : th != null && v >= th ? styles.barAbove : styles.barBelow;
            return <rect key={b.start} x={x(t) + 0.1} y={100 - v * 100} width={0.8} height={v * 100} className={tone} />;
          })}
          {timeline.sessions
            .filter((s) => s.threshold != null)
            .map((s) => (
              <line
                key={`th-${s.publisherEpoch}`}
                x1={x(Date.parse(s.firstSampleAt))}
                x2={x(Date.parse(s.lastSampleAt) + bucketMs)}
                y1={100 - (s.threshold as number) * 100}
                y2={100 - (s.threshold as number) * 100}
                className={styles.thresholdLine}
                vectorEffect="non-scaling-stroke"
              />
            ))}
          <line x1={0} x2={n} y1={100 - critical * 100} y2={100 - critical * 100} className={styles.criticalLine} vectorEffect="non-scaling-stroke" />
        </svg>
      </div>

      {timeline.labels.map((label) => {
        const off = hidden.has(label);
        return (
          <LabelLane key={label} timeline={timeline} label={label} off={off} n={n} x={x} onToggle={() => onToggleLabel(label)} />
        );
      })}

      <span className={styles.trackLabel}>Incidents</span>
      <div className={styles.laneTrack}>
        {timeline.incidents.map((f) => {
          const left = axisPct(Date.parse(f.raisedAt), from, to);
          const right = axisPct(incidentEnd(f, to), from, to);
          const mutedEnd = f.mutedUntil && f.resolution === 'dismissed' ? axisPct(Date.parse(f.mutedUntil), from, to) : null;
          return (
            <span key={f.id}>
              {mutedEnd !== null && mutedEnd > right ? (
                <span
                  className={styles.mute}
                  style={{ left: `${right}%`, width: `${mutedEnd - right}%` }}
                  title={`Muted until ${formatInstant(f.mutedUntil as string)}${f.muteScope ? ` (${f.muteScope})` : ''}`}
                />
              ) : null}
              <button
                type="button"
                className={`${styles.incident} ${incidentTone(f)} ${f.id === selected ? styles.incidentSelected : ''}`}
                style={{ left: `${left}%`, width: `${Math.max(1.5, right - left)}%` }}
                aria-pressed={f.id === selected}
                title={`Flag #${f.id} · ${f.mode} · ${f.state}${f.trips && f.trips > 1 ? ` · ${f.trips} trips` : ''}`}
                onClick={() => onSelect(f)}
              >
                #{f.id}
                {f.trips && f.trips > 1 ? ` ×${f.trips}` : ''}
              </button>
            </span>
          );
        })}
      </div>

      <span className={styles.trackLabel}>Moderation</span>
      <div className={styles.markTrack}>
        {marks.map((m, i) => (
          <span
            key={`${m.at}-${i}`}
            className={`${styles.mark} ${m.tone}`}
            style={{ left: `${axisPct(m.at, from, to)}%` }}
            title={m.title}
          >
            {m.glyph}
          </span>
        ))}
      </div>

      <span />
      <div className={styles.axis} aria-hidden="true">
        {ticks.map((t) => (
          <span key={t.at} style={{ left: `${axisPct(t.at, from, to)}%` }}>
            {t.label}
          </span>
        ))}
      </div>
    </div>
  );
}

function LabelLane({
  timeline,
  label,
  off,
  n,
  x,
  onToggle,
}: {
  timeline: Timeline;
  label: string;
  off: boolean;
  n: number;
  x: (t: number) => number;
  onToggle: () => void;
}) {
  const { floor } = timeline.levels;
  return (
    <>
      <label className={styles.laneLabel} title={label}>
        <input type="checkbox" checked={!off} onChange={onToggle} />
        <span>{shortLabel(label)}</span>
      </label>
      <div className={off ? styles.laneOff : styles.lane}>
        <svg role="img" aria-label={`${label} score per bucket`} viewBox={`0 0 ${n} 1`} preserveAspectRatio="none" className={styles.chartSvg}>
          {timeline.buckets.map((b) => {
            const v = b.scores[label] ?? 0;
            if (v < floor) return null;
            const t = Date.parse(b.start);
            const th = thresholdAt(timeline, t);
            const strength = Math.min(1, 0.25 + (v - floor) * 1.5);
            return (
              <rect
                key={b.start}
                x={x(t)}
                y={0}
                width={1.02}
                height={1}
                className={th != null && v >= th ? styles.cellAbove : styles.cellBelow}
                opacity={strength}
              />
            );
          })}
        </svg>
      </div>
    </>
  );
}

function incidentTone(f: ContentFlag): string {
  if (f.mode === 'observe') return styles.incidentObserved;
  if (f.state === 'open') return styles.incidentOpen;
  if (f.state === 'acknowledged') return styles.incidentAck;
  return styles.incidentResolved;
}

function eventGlyph(type: string): { glyph: string; tone: string } {
  switch (type) {
    case 'content_flag.raised':
      return { glyph: '!', tone: styles.markBad };
    case 'content_flag.reminded':
      return { glyph: '↻', tone: styles.markWarn };
    case 'content_flag.resolved':
      return { glyph: '○', tone: styles.markPlain };
    case 'content_flag.observed':
      return { glyph: '·', tone: styles.markPlain };
    case 'broadcast.killed':
    case 'ban.created':
      return { glyph: '✕', tone: styles.markBad };
    case 'broadcast.publisher_away':
      return { glyph: '‖', tone: styles.markPlain };
    case 'broadcast.publisher_back':
      return { glyph: '▸', tone: styles.markPlain };
    default:
      return { glyph: '•', tone: styles.markPlain };
  }
}
