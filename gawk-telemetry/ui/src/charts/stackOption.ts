import { hatchPattern, ZOOM_ID, type EChartsOption } from './echarts.ts';
import type { Palette } from './palette.ts';
import { niceMax } from './stack.ts';

// The option a stack member is drawn with (R36, docs/86 D4/D5), pure so the
// rules in it are testable: no text in a lane, gaps as breaks, and the
// stack's zoom carried into every rebuild.

export interface StackSeries {
  key: string;
  label: string;
  /** `[timestampMs, value | null]`. A null is a BREAK, not a zero. */
  data: Array<[number, number | null]>;
  color?: string;
  area?: boolean;
  dashed?: boolean;
  step?: boolean;
}

export interface StackBand {
  fromMs: number;
  toMs: number;
  /** veil: not joined, left, tab hidden. dip: a collapse. */
  kind: 'veil' | 'dip';
  label: string;
}

export interface StackMark {
  atMs: number;
  /** rehome: the relay moved it. note: an operator's annotation. */
  kind: 'rehome' | 'note';
  label: string;
}

/** The share of the range a stack shows, in percent. */
export interface StackZoom {
  start: number;
  end: number;
}

const FULL: StackZoom = { start: 0, end: 100 };

export interface StackOptionInput {
  series: StackSeries[];
  bands?: StackBand[];
  marks?: StackMark[];
  ticks?: number[];
  fromMs: number;
  toMs: number;
  yLabels?: boolean;
  quiet?: boolean;
}

export const Y_GUTTER = 40;

export function buildStackOption(
  p: Palette,
  o: StackOptionInput,
  zoom: StackZoom = FULL,
): EChartsOption {
  let max = 0;
  for (const s of o.series) for (const [, v] of s.data) if (v !== null && v > max) max = v;
  const yMax = niceMax(max * 1.08);

  const veil = hatchPattern(p.hatchLine);
  const markArea = o.bands?.length
    ? {
        silent: true,
        label: { show: false },
        itemStyle: { opacity: 1 },
        data: o.bands.map((b) => [
          { xAxis: b.fromMs, name: b.label, itemStyle: { color: b.kind === 'dip' ? p.badBg : veil } },
          { xAxis: b.toMs },
        ]),
      }
    : undefined;

  // Floor ticks: a segment from y=0 to ~9 % of the scale. Bare lines with no
  // label — the readout names the event under the crosshair.
  const tickH = yMax * 0.09;
  const lines: unknown[] = [];
  for (const m of o.marks ?? []) {
    lines.push({
      xAxis: m.atMs,
      name: m.label,
      lineStyle: { color: m.kind === 'note' ? p.accent : p.text3, type: 'dashed', width: 1 },
    });
  }
  for (const t of o.ticks ?? []) {
    lines.push([
      { coord: [t, 0], lineStyle: { color: p.text3, type: 'solid', width: 1 } },
      { coord: [t, tickH] },
    ]);
  }
  const markLine = lines.length
    ? { silent: true, symbol: ['none', 'none'], label: { show: false }, animation: false, data: lines }
    : undefined;

  return {
    animation: false,
    textStyle: { fontFamily: p.sans },
    grid: {
      left: o.yLabels ? Y_GUTTER : 0,
      right: 0,
      top: o.quiet ? 4 : 8,
      bottom: o.quiet ? 4 : 8,
      containLabel: false,
    },
    // The tooltip exists only for its crosshair: the values are in the
    // readout rail, which never moves.
    tooltip: {
      trigger: 'axis',
      showContent: false,
      // snap: false — the crosshair follows the mouse, not the nearest point
      // of whichever member it is over. Snapping made a lane that had ended
      // drag the WHOLE stack's instant back to its last sample.
      axisPointer: { type: 'line', snap: false, lineStyle: { color: p.text, width: 1, opacity: 0.7, type: 'solid' } },
    },
    // The tooltip forces snapping on; only the axis's own axisPointer can
    // turn it off (echarts modelHelper.makeAxisPointerModel).
    xAxis: { type: 'time', min: o.fromMs, max: o.toMs, show: false, axisPointer: { snap: false } },
    yAxis: {
      type: 'value',
      min: 0,
      max: yMax,
      show: !!o.yLabels,
      splitNumber: 3,
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { color: p.text3, fontSize: 10, fontFamily: p.sans, width: Y_GUTTER - 8, overflow: 'truncate' },
      splitLine: { show: !!o.yLabels, lineStyle: { color: p.line } },
    },
    // The stack's CURRENT zoom: setOption replaces the option outright
    // (notMerge), so a zoom left out here is a zoom reset on every rebuild.
    dataZoom: [{ id: ZOOM_ID, type: 'inside', filterMode: 'none', start: zoom.start, end: zoom.end }],
    series: o.series.map((s, i) => {
      const color = s.color ?? p.series[i % p.series.length];
      return {
        type: 'line',
        name: s.label,
        showSymbol: false,
        // No hover dot either: the value at the crosshair is in the rail.
        symbol: 'none',
        // Gaps are BREAKS (UD9); stated explicitly because turning it on would
        // silently invent data.
        connectNulls: false,
        step: s.step ? 'end' : undefined,
        lineStyle: { width: o.quiet ? 1 : 1.4, color, type: s.dashed ? 'dashed' : 'solid' },
        itemStyle: { color },
        areaStyle: s.area ? { color: p.accentBg, opacity: 1 } : undefined,
        data: s.data,
        // Marks ride the FIRST series only; ECharts would otherwise draw one
        // copy per series.
        markArea: i === 0 ? markArea : undefined,
        markLine: i === 0 ? markLine : undefined,
      };
    }),
  } as EChartsOption;
}

/**
 * An empty member still joins the stack's crosshair and zoom, so the readout
 * rail and the shared axis stay meaningful across it.
 */
export function emptyStackOption(p: Palette, fromMs: number, toMs: number, zoom: StackZoom = FULL): EChartsOption {
  return {
    animation: false,
    grid: { left: 0, right: 0, top: 0, bottom: 0 },
    tooltip: {
      trigger: 'axis',
      showContent: false,
      axisPointer: { type: 'line', snap: false, lineStyle: { color: p.text, width: 1, opacity: 0.4, type: 'solid' } },
    },
    xAxis: { type: 'time', min: fromMs, max: toMs, show: false, axisPointer: { snap: false } },
    yAxis: { type: 'value', show: false, min: 0, max: 1 },
    // The stack's CURRENT zoom: setOption replaces the option outright
    // (notMerge), so a zoom left out here is a zoom reset on every rebuild.
    dataZoom: [{ id: ZOOM_ID, type: 'inside', filterMode: 'none', start: zoom.start, end: zoom.end }],
    series: [{ type: 'line', data: [], showSymbol: false }],
  } as EChartsOption;
}
