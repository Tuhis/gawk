import { useMemo } from 'react';

import type { Point } from '../lib/history.ts';
import { EMPTY, num } from '../lib/format.ts';
import styles from './TimelineChart.module.css';

export interface SeriesSpec {
  key: string;
  label: string;
  /** A CSS colour, usually a var(). */
  color: string;
  /** Dashed lines read as "reference", not "measurement" — used for targets. */
  dashed?: boolean;
}

interface Props {
  title: string;
  unit: string;
  points: Point[];
  series: SeriesSpec[];
  /** The full window the graph claims to cover, so a short buffer can say so. */
  windowMs: number;
  /** Floor for the y-axis top, so a flat-zero series is not auto-scaled to noise. */
  minTop?: number;
  height?: number;
  /**
   * Metric (0/1) marking spans the chart should shade as "do not read the lines
   * here". Used for tab visibility: a hidden tab stops firing rAF, so a
   * viewer's rendered line collapses for a reason that has nothing to do with
   * the stream. Shading says which, instead of leaving a cliff to be guessed at.
   */
  shadeKey?: string;
  shadeLabel?: string;
}

const PAD_L = 34;
const PAD_R = 6;
const PAD_T = 6;
const PAD_B = 14;
const VIEW_W = 600;

/**
 * A small multi-series line chart, plain SVG and no charting library — the page
 * may not fetch anything at runtime, and this is far less code than a
 * dependency would be.
 *
 * The layout is FIXED: the SVG has a constant viewBox and the container a
 * constant height, so a series appearing, disappearing or rescaling never
 * changes the page's geometry. That is the whole reason the axis labels are
 * inside the SVG rather than being DOM text that would reflow.
 */
export function TimelineChart({
  title,
  unit,
  points,
  series,
  windowMs,
  minTop = 1,
  height = 96,
  shadeKey,
  shadeLabel,
}: Props) {
  const model = useMemo(() => {
    if (points.length === 0) return null;

    const tEnd = points[points.length - 1].t;
    // The x-axis is ALWAYS the full window, never just the data's extent. A
    // buffer holding 40 s must draw 40 s of line against 10 minutes of axis, so
    // "we have just started watching" is visible rather than being stretched
    // into a full-width story.
    const tStart = tEnd - windowMs;

    let top = minTop;
    for (const p of points) {
      for (const s of series) {
        const v = p.v[s.key];
        if (Number.isFinite(v) && v > top) top = v;
      }
    }
    // Round the axis up so it does not twitch on every new maximum.
    const magnitude = Math.pow(10, Math.floor(Math.log10(top)));
    top = Math.ceil(top / magnitude) * magnitude;

    const x = (t: number) =>
      PAD_L + ((t - tStart) / windowMs) * (VIEW_W - PAD_L - PAD_R);
    const y = (v: number) =>
      PAD_T + (1 - v / top) * (height - PAD_T - PAD_B);

    const paths = series.map((s) => {
      // Gaps are BREAKS, not interpolations: a metric that stopped being
      // reported did not glide to its next value, and drawing it as though it
      // did would invent data. Each contiguous run is its own subpath.
      const runs: string[] = [];
      let run: string[] = [];
      for (const p of points) {
        const v = p.v[s.key];
        if (!Number.isFinite(v)) {
          if (run.length) runs.push(run.join(' '));
          run = [];
          continue;
        }
        run.push(`${run.length ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(v).toFixed(1)}`);
      }
      if (run.length) runs.push(run.join(' '));
      const last = [...points].reverse().find((p) => Number.isFinite(p.v[s.key]));
      return { spec: s, d: runs.join(' '), latest: last?.v[s.key] };
    });

    // Contiguous runs where the shade metric is truthy, as x-ranges. Built from
    // the same point list, so a span can never drift out of step with the lines
    // it qualifies.
    const shades: Array<{ x: number; w: number }> = [];
    if (shadeKey) {
      let runStart: number | null = null;
      for (const p of points) {
        const on = !!p.v[shadeKey];
        if (on && runStart === null) runStart = p.t;
        if (!on && runStart !== null) {
          shades.push({ x: x(runStart), w: Math.max(1, x(p.t) - x(runStart)) });
          runStart = null;
        }
      }
      if (runStart !== null) {
        shades.push({ x: x(runStart), w: Math.max(1, x(tEnd) - x(runStart)) });
      }
    }

    return {
      top,
      paths,
      shades,
      gridY: [0, 0.5, 1].map((f) => PAD_T + f * (height - PAD_T - PAD_B)),
    };
  }, [points, series, windowMs, minTop, height, shadeKey]);

  return (
    <figure className={styles.chart} style={{ ['--chart-h' as string]: `${height}px` }}>
      <figcaption className={styles.head}>
        <span className={styles.title}>{title}</span>
        <span className={styles.legend}>
          {shadeLabel && model && model.shades.length > 0 && (
            <span className={styles.legendItem}>
              <span className={`${styles.swatch} ${styles.shadeSwatch}`} aria-hidden />
              {shadeLabel}
            </span>
          )}
          {series.map((s) => {
            const latest = model?.paths.find((p) => p.spec.key === s.key)?.latest;
            return (
              <span key={s.key} className={styles.legendItem}>
                <span
                  className={styles.swatch}
                  style={{ background: s.color, opacity: s.dashed ? 0.55 : 1 }}
                  aria-hidden
                />
                {s.label}
                <span className={`${styles.legendVal} tnum`}>
                  {latest === undefined ? EMPTY : num(latest, latest < 10 ? 1 : 0)}
                </span>
              </span>
            );
          })}
        </span>
      </figcaption>

      <svg
        className={styles.svg}
        viewBox={`0 0 ${VIEW_W} ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`${title} over the last ${Math.round(windowMs / 60000)} minutes`}
      >
        {/* Shading goes down FIRST so the lines stay legible on top of it. */}
        {model?.shades.map((s, i) => (
          <rect
            key={`shade-${i}`}
            x={s.x}
            y={PAD_T}
            width={s.w}
            height={height - PAD_T - PAD_B}
            className={styles.shade}
          />
        ))}
        {model?.gridY.map((gy, i) => (
          <line
            key={i}
            x1={PAD_L}
            x2={VIEW_W - PAD_R}
            y1={gy}
            y2={gy}
            className={styles.grid}
            vectorEffect="non-scaling-stroke"
          />
        ))}
        {model && (
          <>
            <text x={4} y={PAD_T + 8} className={styles.axis}>
              {num(model.top)}
            </text>
            <text x={4} y={height - PAD_B + 4} className={styles.axis}>
              0
            </text>
            <text x={PAD_L} y={height - 3} className={styles.axis}>
              −{Math.round(windowMs / 60000)}m
            </text>
            <text x={VIEW_W - PAD_R} y={height - 3} className={styles.axisEnd}>
              now
            </text>
          </>
        )}
        {model?.paths.map(({ spec, d }) => (
          <path
            key={spec.key}
            d={d}
            fill="none"
            stroke={spec.color}
            strokeWidth={1.5}
            strokeDasharray={spec.dashed ? '3 3' : undefined}
            strokeLinejoin="round"
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
          />
        ))}
      </svg>
      <span className={styles.unit}>{unit}</span>
    </figure>
  );
}
