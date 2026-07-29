import { useMemo } from 'react';

import { AXIS_STYLE, GRID, SERIES_COLORS, type EChartsOption } from './echarts.ts';
import { useChart } from './useChart.ts';
import { axisTime, tooltipTime } from '../lib/format.ts';
import styles from './TimeChart.module.css';

// The one chart component every historical surface uses.
//
// It replaces `TimelineChart.tsx`'s hand-rolled SVG, and UD11 is explicit that
// two of that component's behaviours must SURVIVE the replacement rather than
// being lost with it:
//
//   * **Gaps are breaks, never interpolations** (UD9). A metric that stopped
//     being reported did not glide to its next value. ECharts draws a break for
//     a `null` value, so the caller passes `null` and this component never
//     "cleans" it.
//   * **A fixed geometry, so nothing reflows as values tick.** The container
//     has a fixed height and the y-axis reserves its label width, which is
//     TH11's no-layout-shift rule at UD14's density.
//
// Everything historical is on an ABSOLUTE time axis (UD5), with the timezone
// stated by the caller's header rather than assumed here.

export interface Series {
  key: string;
  label: string;
  /** `[timestampMs, value | null]`. A null is a BREAK, not a zero. */
  data: Array<[number, number | null]>;
  color?: string;
  unit?: string;
  dashed?: boolean;
  /** Draw as a step line: correct for a state that holds until it changes. */
  step?: boolean;
  /** Right-hand axis, for a series whose unit differs (TH5). */
  axis?: 0 | 1;
  area?: boolean;
}

/** A shaded span: a hidden tab, a dip episode, a degraded band. */
export interface Band {
  fromMs: number;
  toMs: number;
  label?: string;
  color?: string;
}

/** A vertical marker: a stored event, an annotation, a re-home. */
export interface Marker {
  atMs: number;
  label: string;
  color?: string;
}

interface Props {
  series: Series[];
  bands?: Band[];
  markers?: Marker[];
  /** The axis extent. Given explicitly so several lanes share one (TH4). */
  fromMs?: number;
  toMs?: number;
  height?: number;
  /** ECharts connect group — one crosshair and one zoom across lanes. */
  group?: string;
  /** Unit labels for the left and right axes. */
  unit?: string;
  unitRight?: string;
  /** Shown under the chart: UD9's "you are looking at worst-of-N" disclosure. */
  note?: string;
  /** Rendered instead of the chart when there is nothing to draw (UD10). */
  empty?: string;
  showZoom?: boolean;
  ariaLabel?: string;
}

export function TimeChart({
  series,
  bands,
  markers,
  fromMs,
  toMs,
  height = 160,
  group,
  unit,
  unitRight,
  note,
  empty,
  showZoom,
  ariaLabel,
}: Props) {
  const option = useMemo<EChartsOption | null>(() => {
    if (series.length === 0) return null;
    const hasRight = series.some((s) => s.axis === 1);

    const markArea = bands?.length
      ? {
          silent: true,
          itemStyle: { opacity: 1 },
          data: bands.map((b) => [
            {
              xAxis: b.fromMs,
              name: b.label,
              itemStyle: { color: b.color ?? 'rgba(110,118,129,0.18)' },
            },
            { xAxis: b.toMs },
          ]),
        }
      : undefined;

    const markLine = markers?.length
      ? {
          silent: false,
          symbol: ['none', 'none'] as [string, string],
          label: {
            show: true,
            formatter: (p: { name?: string }) => p.name ?? '',
            color: '#9aa0ab',
            fontSize: 9,
            rotate: 90,
            align: 'left' as const,
            verticalAlign: 'middle' as const,
            padding: [0, 0, 0, 4] as [number, number, number, number],
          },
          data: markers.map((m) => ({
            xAxis: m.atMs,
            name: m.label,
            lineStyle: { color: m.color ?? '#6e7681', type: 'dashed' as const, width: 1 },
          })),
        }
      : undefined;

    return {
      animation: false,
      grid: { ...GRID, right: hasRight ? 52 : GRID.right },
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'line', lineStyle: { color: '#58a6ff', width: 1 } },
        backgroundColor: '#16181d',
        borderColor: '#333846',
        textStyle: { color: '#e9eaee', fontSize: 11 },
        // The tooltip is where absolute time actually pays off: it is what an
        // operator copies into a relay log search.
        formatter: (params: unknown) => {
          const rows = Array.isArray(params) ? params : [params];
          const first = rows[0] as { value?: [number, number | null] } | undefined;
          const at = first?.value?.[0];
          const head = typeof at === 'number' ? tooltipTime(at) : '';
          const body = rows
            .map((r) => {
              const p = r as { marker?: string; seriesName?: string; value?: [number, number | null] };
              const v = p.value?.[1];
              // An absent reading renders as an em dash, never as 0.
              return `${p.marker ?? ''}${p.seriesName ?? ''} <b>${v === null || v === undefined ? '—' : formatValue(v)}</b>`;
            })
            .join('<br/>');
          return `${head}<br/>${body}`;
        },
      },
      legend:
        series.length > 1
          ? {
              type: 'scroll',
              top: 0,
              right: 8,
              itemWidth: 10,
              itemHeight: 2,
              textStyle: { color: '#9aa0ab', fontSize: 10 },
            }
          : undefined,
      xAxis: {
        type: 'time',
        min: fromMs,
        max: toMs,
        ...AXIS_STYLE,
        axisLabel: { ...AXIS_STYLE.axisLabel, formatter: axisTime, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: [
        {
          type: 'value',
          name: unit,
          nameTextStyle: { color: '#6b7280', fontSize: 9, align: 'left' },
          nameGap: 6,
          ...AXIS_STYLE,
          // A fixed label width is what stops the plot area — and therefore the
          // whole row — from shifting as a value crosses 9.9 → 10.
          axisLabel: { ...AXIS_STYLE.axisLabel, width: 40, overflow: 'truncate' },
          scale: false,
          min: 0,
        },
        ...(hasRight
          ? [
              {
                type: 'value' as const,
                name: unitRight,
                nameTextStyle: { color: '#6b7280', fontSize: 9 },
                ...AXIS_STYLE,
                splitLine: { show: false },
              },
            ]
          : []),
      ],
      dataZoom: showZoom
        ? [
            { type: 'inside', filterMode: 'none' },
            { type: 'slider', height: 14, bottom: 2, filterMode: 'none' },
          ]
        : [{ type: 'inside', filterMode: 'none' }],
      series: series.map((s, i) => ({
        type: 'line',
        name: s.label,
        showSymbol: false,
        // Gaps are BREAKS. `connectNulls: false` is the default and is the
        // behaviour UD9 requires; it is stated explicitly because turning it on
        // would silently invent data.
        connectNulls: false,
        step: s.step ? ('end' as const) : undefined,
        yAxisIndex: s.axis === 1 ? 1 : 0,
        lineStyle: {
          width: 1.5,
          color: s.color ?? SERIES_COLORS[i % SERIES_COLORS.length],
          type: s.dashed ? ('dashed' as const) : ('solid' as const),
        },
        itemStyle: { color: s.color ?? SERIES_COLORS[i % SERIES_COLORS.length] },
        areaStyle: s.area ? { opacity: 0.12 } : undefined,
        data: s.data,
        // Marks ride the FIRST series only; ECharts would otherwise draw one
        // copy per series and turn a single event into a picket fence.
        markArea: i === 0 ? markArea : undefined,
        markLine: i === 0 ? markLine : undefined,
      })),
    } as EChartsOption;
  }, [series, bands, markers, fromMs, toMs, unit, unitRight, showZoom]);

  const ref = useChart(option, { group });

  return (
    <figure className={styles.wrap} style={{ ['--chart-h' as string]: `${height}px` }}>
      {/* The container keeps its height whether or not there is data, so an
          empty state and a populated one occupy the same space. */}
      <div className={styles.canvas} ref={ref} role="img" aria-label={ariaLabel} />
      {option === null && <p className={styles.empty}>{empty ?? 'No data in this range.'}</p>}
      {note && <figcaption className={styles.note}>{note}</figcaption>}
    </figure>
  );
}

function formatValue(v: number): string {
  if (!Number.isFinite(v)) return '—';
  if (Math.abs(v) >= 1000) return v.toFixed(0);
  if (Math.abs(v) >= 10) return v.toFixed(1);
  return v.toFixed(2);
}
