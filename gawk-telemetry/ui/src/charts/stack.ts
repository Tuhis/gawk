import { useCallback, useEffect, useRef, useState } from 'react';

import type { EChartsType } from './echarts.ts';

// A chart STACK (R36 item 2, docs/86 TU3): several ECharts instances joined by
// `echarts.connect()` into one crosshair and one zoom, read through ONE time
// axis and a readout rail instead of a floating tooltip.
//
// R31's lanes each carried a tooltip that ECharts placed relative to the
// cursor and flipped near an edge, so its position depended on which lane the
// cursor was over and which way it arrived — read as a bug, not a policy. Here
// the stack itself owns two numbers, the crosshair instant and the visible
// window, and every lane prints its own value at that instant in a fixed
// column. Comparing lanes becomes a vertical glance.

export interface StackView {
  /** The crosshair's instant, or null when the cursor is off the stack. */
  cursorMs: number | null;
  /** The visible window, after any zoom. */
  viewFrom: number;
  viewTo: number;
}

export interface Stack extends StackView {
  /** Pass to every member chart's `onInit`. */
  register: (chart: EChartsType) => void;
}

interface AxisInfo {
  axesInfo?: Array<{ value?: number }>;
}

export function useStack(fromMs: number, toMs: number): Stack {
  const [cursorMs, setCursor] = useState<number | null>(null);
  const [view, setView] = useState({ from: fromMs, to: toMs });
  const frame = useRef<number | null>(null);
  const pending = useRef<number | null>(null);
  const range = useRef({ fromMs, toMs });
  // The share of the range the zoom shows, kept as fractions: a live
  // broadcast's range grows every refresh, and a zoom held as instants would
  // drift off the end.
  const zoom = useRef<{ start: number; end: number }>({ start: 0, end: 100 });

  useEffect(() => {
    range.current = { fromMs, toMs };
    const span = toMs - fromMs;
    setView({
      from: fromMs + (span * zoom.current.start) / 100,
      to: fromMs + (span * zoom.current.end) / 100,
    });
  }, [fromMs, toMs]);

  useEffect(
    () => () => {
      if (frame.current !== null) cancelAnimationFrame(frame.current);
    },
    [],
  );

  const charts = useRef<EChartsType[]>([]);

  const schedule = useCallback((v: number | null) => {
    pending.current = v;
    if (frame.current !== null) return;
    frame.current = requestAnimationFrame(() => {
      frame.current = null;
      setCursor(pending.current);
    });
  }, []);

  const register = useCallback(
    (chart: EChartsType) => {
      charts.current = charts.current.filter((c) => !c.isDisposed());
      charts.current.push(chart);

      // Cursor moves arrive per mouse event; the readouts repaint once a
      // frame. `connect` re-dispatches the pointer to every member, and the
      // members the mouse is NOT over report an empty axesInfo — so an empty
      // one is not "the cursor left", only `globalout` is.
      chart.on('updateAxisPointer', (e: unknown) => {
        const v = (e as AxisInfo).axesInfo?.[0]?.value;
        if (typeof v === 'number') schedule(v);
      });
      chart.getZr().on('globalout', () => schedule(null));

      // Zoom is synced here rather than left to `connect`, which relays an
      // inside-zoom but not the slider's: one zoom, applied to every member.
      chart.on('datazoom', () => {
        const dz = (chart.getOption() as { dataZoom?: Array<{ start?: number; end?: number }> }).dataZoom?.[0];
        if (!dz) return;
        const start = dz.start ?? 0;
        const end = dz.end ?? 100;
        // The echo of our own relay (a member reporting the zoom it was just
        // given) changes nothing; stopping on it is what ends the relay.
        if (Math.abs(start - zoom.current.start) < 1e-6 && Math.abs(end - zoom.current.end) < 1e-6) return;
        zoom.current = { start, end };
        for (const other of charts.current) {
          if (other === chart || other.isDisposed()) continue;
          other.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, start, end });
        }
        const { fromMs: f, toMs: t } = range.current;
        setView({ from: f + ((t - f) * start) / 100, to: f + ((t - f) * end) / 100 });
      });

      // A member mounted after a zoom (a lane that appeared on refresh, a
      // chart shown by "show more") joins at the stack's current zoom.
      const { start, end } = zoom.current;
      if (start !== 0 || end !== 100) {
        requestAnimationFrame(() => {
          if (!chart.isDisposed()) chart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, start, end });
        });
      }
    },
    [schedule],
  );

  return { cursorMs, viewFrom: view.from, viewTo: view.to, register };
}

/**
 * The value a series held at `t`: the last point at or before it, and only if
 * that point is recent enough to still describe `t`. A reading older than
 * `maxAgeMs` is ABSENT, not stale-but-shown — UD9's gap, carried into the
 * readout.
 */
export function valueAt(points: Array<[number, number | null]>, t: number, maxAgeMs: number): number | null {
  let lo = 0;
  let hi = points.length - 1;
  let idx = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (points[mid][0] <= t) {
      idx = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  if (idx < 0) return null;
  const [at, v] = points[idx];
  if (t - at > maxAgeMs) return null;
  return v;
}

/**
 * The typical spacing of a series' points: its median gap. A downsampled lane
 * (worst of every N samples) is spaced wider than its source's cadence, and a
 * readout must not call a point "too old" just because the reduction moved
 * its neighbour away.
 */
export function stepOf(points: Array<[number, number | null]>): number {
  const gaps: number[] = [];
  for (let i = 1; i < points.length && gaps.length < 600; i++) gaps.push(points[i][0] - points[i - 1][0]);
  if (gaps.length === 0) return 0;
  gaps.sort((a, b) => a - b);
  return gaps[gaps.length >> 1];
}

/** The last non-null value in a series: a lane's reading when no crosshair is up. */
export function lastValue(points: Array<[number, number | null]>): number | null {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = points[i][1];
    if (v !== null) return v;
  }
  return null;
}

const STEPS = [
  1_000, 2_000, 5_000, 10_000, 15_000, 30_000, 60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000,
  3_600_000, 7_200_000, 10_800_000, 21_600_000, 43_200_000, 86_400_000, 172_800_000, 604_800_000,
];

export interface Tick {
  atMs: number;
  /** 0–100, across the visible window. */
  pct: number;
  label: string;
}

/**
 * Round-instant ticks across a window, at most `max` of them. Aligned to the
 * LOCAL clock (an hour tick lands on :00 here, not in UTC), because every
 * label on this page is local time with the zone stated once (UD5).
 */
export function timeTicks(fromMs: number, toMs: number, max = 8): Tick[] {
  const span = toMs - fromMs;
  if (!(span > 0)) return [];
  const step = STEPS.find((s) => span / s <= max) ?? STEPS[STEPS.length - 1];
  const tz = new Date(fromMs).getTimezoneOffset() * 60_000;
  let t = Math.ceil((fromMs - tz) / step) * step + tz;
  const out: Tick[] = [];
  for (; t <= toMs && out.length < 64; t += step) {
    out.push({ atMs: t, pct: ((t - fromMs) / span) * 100, label: tickLabel(t, step) });
  }
  return out;
}

function tickLabel(t: number, step: number): string {
  const d = new Date(t);
  if (step >= 86_400_000) {
    return d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' });
  }
  const midnight = d.getHours() === 0 && d.getMinutes() === 0 && d.getSeconds() === 0;
  if (midnight) return d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    ...(step < 60_000 ? { second: '2-digit' } : {}),
    hour12: false,
  });
}

/** The crosshair's own label: the instant to the second. */
export function cursorLabel(t: number): string {
  return new Date(t).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}

/** A readout number: as many digits as the magnitude deserves. */
export function readoutValue(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return '—';
  const a = Math.abs(v);
  if (a >= 10_000) return Math.round(v).toLocaleString();
  if (a >= 100) return v.toFixed(0);
  if (a >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

/** A rounded-up axis maximum, so a lane's scale is a number a person can read. */
export function niceMax(v: number): number {
  if (!(v > 0)) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (v <= m * exp) return m * exp;
  }
  return 10 * exp;
}
