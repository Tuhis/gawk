import { useMemo } from 'react';

import { hatchPattern, ZOOM_ID, type EChartsOption, type EChartsType } from './echarts.ts';
import { usePalette, type Palette } from './palette.ts';
import { niceMax } from './stack.ts';
import { useChart } from './useChart.ts';
import styles from './StackChart.module.css';

// One member of a chart stack (R36, docs/86 TU3): the plot and nothing else.
//
// **No text is ever drawn inside it.** R31's lanes labelled every markArea and
// markLine at the lane's top edge, and on a real broadcast the labels overlapped
// into runs like "taltatdipbhidipdip…". Here a band is a fill, a re-home or a
// note is a bare line, and a stored event is a short tick on the lane's FLOOR;
// their names live in the readout rail at the crosshair and in the legend.
// The time axis is the stack's, drawn once above it (`TimeAxis`), so a lane
// spends its height on data instead of repeating axis furniture.
//
// What survives from TimeChart, because UD9/UD11 require it:
//   * **Gaps are breaks** — a null is a discontinuity, `connectNulls` stays off.
//   * **Fixed geometry** — the container's height never depends on the data.

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

interface Props {
  series: StackSeries[];
  bands?: StackBand[];
  marks?: StackMark[];
  /** Stored events, drawn as floor ticks. */
  ticks?: number[];
  fromMs: number;
  toMs: number;
  height: number;
  group: string;
  onInit?: (chart: EChartsType) => void;
  /** Left gutter with y-axis labels (session charts). Lanes have none. */
  yLabels?: boolean;
  /** Text shown over an empty plot — the only text a plot may hold (UD10). */
  empty?: string;
  ariaLabel?: string;
  /** Sub-lanes are slimmer and quieter. */
  quiet?: boolean;
}

export const Y_GUTTER = 40;

export function StackChart({
  series,
  bands,
  marks,
  ticks,
  fromMs,
  toMs,
  height,
  group,
  onInit,
  yLabels,
  empty,
  ariaLabel,
  quiet,
}: Props) {
  const p = usePalette();
  const hasData = series.some((s) => s.data.some(([, v]) => v !== null));

  const option = useMemo<EChartsOption | null>(
    () => (hasData ? buildOption(p, { series, bands, marks, ticks, fromMs, toMs, yLabels, quiet }) : emptyOption(p, fromMs, toMs)),
    [p, hasData, series, bands, marks, ticks, fromMs, toMs, yLabels, quiet],
  );

  const ref = useChart(option, { group, onInit });

  return (
    <div className={styles.wrap} style={{ height }}>
      <div className={styles.canvas} ref={ref} role="img" aria-label={ariaLabel} />
      {!hasData && empty ? <p className={styles.empty}>{empty}</p> : null}
    </div>
  );
}

function buildOption(
  p: Palette,
  o: Pick<Props, 'series' | 'bands' | 'marks' | 'ticks' | 'fromMs' | 'toMs' | 'yLabels' | 'quiet'>,
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
    dataZoom: [{ id: ZOOM_ID, type: 'inside', filterMode: 'none' }],
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
function emptyOption(p: Palette, fromMs: number, toMs: number): EChartsOption {
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
    dataZoom: [{ id: ZOOM_ID, type: 'inside', filterMode: 'none' }],
    series: [{ type: 'line', data: [], showSymbol: false }],
  } as EChartsOption;
}
