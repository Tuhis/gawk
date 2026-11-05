import { useMemo } from 'react';

import { axisStyle, hatchPattern, ZOOM_ID, type EChartsOption } from './echarts.ts';
import { usePalette } from './palette.ts';
import { useChart } from './useChart.ts';
import { axisTime, tooltipTime } from '../lib/format.ts';
import styles from './TimeChart.module.css';

// The single-chart component: the explorer, the fleet trend, a SQL result.
// (Stacks — the broadcast lanes and the session timeline — use StackChart and
// a shared axis instead; see charts/stack.ts.)
//
// UD11's two behaviours survive every revision:
//
//   * **Gaps are breaks, never interpolations** (UD9). A metric that stopped
//     being reported did not glide to its next value. ECharts draws a break for
//     a `null` value, so the caller passes `null` and this component never
//     "cleans" it.
//   * **A fixed geometry, so nothing reflows as values tick.** The container
//     has a fixed height and the y-axis reserves its label width, which is
//     TH11's no-layout-shift rule at UD14's density.
//
// R36 (docs/86 TU3): the tooltip is PINNED to the plot's top-right corner and
// confined to it. R31 let ECharts place it beside the cursor and flip it near
// an edge, which read as a bug rather than a placement. Colours come from the
// theme's tokens; marks carry no text (their names are in the tooltip).
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
  /**
   * Points too thin to draw as a line (a trend bucket under five sessions),
   * shown as hollow, unconnected circles: present, but claiming nothing.
   */
  hollow?: Array<[number, number | null]>;
  /**
   * Mark every point with a solid dot: right for bucketed aggregates, where a
   * bucket with no neighbour is still a claim — a bare line would not draw it.
   */
  points?: boolean;
}

/** A shaded span: a hidden tab, a dip episode, a degraded band. */
export interface Band {
  fromMs: number;
  toMs: number;
  label?: string;
  /** veil: hatched "was not here". Otherwise a tint (severity or neutral). */
  kind?: 'veil' | 'dip' | 'tint';
}

/** A vertical marker: a stored event, an annotation, a re-home. */
export interface Marker {
  atMs: number;
  label: string;
  kind?: 'event' | 'note';
}

interface Props {
  series: Series[];
  bands?: Band[];
  markers?: Marker[];
  /** The axis extent. */
  fromMs?: number;
  toMs?: number;
  height?: number;
  /** ECharts connect group. */
  group?: string;
  /** Unit labels for the left and right axes. */
  unit?: string;
  unitRight?: string;
  /** Shown under the chart: UD9's "you are looking at worst-of-N" disclosure. */
  note?: string;
  /** Rendered instead of the chart when there is nothing to draw (UD10). */
  empty?: string;
  /** Rendered OVER a chart that has points but no line (all thin, say). */
  overlay?: React.ReactNode;
  showZoom?: boolean;
  ariaLabel?: string;
  /** An HTML legend above the plot. Defaults to on for two or more series. */
  legend?: boolean;
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
  overlay,
  showZoom,
  ariaLabel,
  legend,
}: Props) {
  const p = usePalette();
  const colorOf = (s: Series, i: number) => s.color ?? p.series[i % p.series.length];

  const option = useMemo<EChartsOption | null>(() => {
    if (series.length === 0) return null;
    const hasRight = series.some((s) => s.axis === 1);
    const ax = axisStyle(p);
    const veil = hatchPattern(p.hatchLine);

    const markArea = bands?.length
      ? {
          silent: true,
          label: { show: false },
          itemStyle: { opacity: 1 },
          data: bands.map((b) => [
            {
              xAxis: b.fromMs,
              name: b.label,
              itemStyle: { color: b.kind === 'veil' ? veil : b.kind === 'dip' ? p.badBg : p.hatch },
            },
            { xAxis: b.toMs },
          ]),
        }
      : undefined;

    const markLine = markers?.length
      ? {
          silent: true,
          symbol: ['none', 'none'] as [string, string],
          label: { show: false },
          data: markers.map((m) => ({
            xAxis: m.atMs,
            name: m.label,
            lineStyle: { color: m.kind === 'note' ? p.accent : p.text3, type: 'dashed' as const, width: 1 },
          })),
        }
      : undefined;

    const lines = series.map((s, i) => ({
      type: 'line',
      name: s.label,
      showSymbol: !!s.points,
      symbol: 'circle',
      symbolSize: 6,
      // Gaps are BREAKS. `connectNulls: false` is the default and is the
      // behaviour UD9 requires; it is stated explicitly because turning it on
      // would silently invent data.
      connectNulls: false,
      step: s.step ? ('end' as const) : undefined,
      yAxisIndex: s.axis === 1 ? 1 : 0,
      lineStyle: { width: 1.5, color: colorOf(s, i), type: s.dashed ? ('dashed' as const) : ('solid' as const) },
      itemStyle: { color: colorOf(s, i) },
      areaStyle: s.area ? { color: p.accentBg, opacity: 1 } : undefined,
      data: s.data,
      // Marks ride the FIRST series only; ECharts would otherwise draw one
      // copy per series and turn a single event into a picket fence.
      markArea: i === 0 ? markArea : undefined,
      markLine: i === 0 ? markLine : undefined,
    }));
    const hollows = series.flatMap((s, i) =>
      s.hollow?.some(([, v]) => v !== null)
        ? [
            {
              type: 'line',
              name: `${s.label} (thin)`,
              data: s.hollow,
              connectNulls: false,
              showSymbol: true,
              symbol: 'circle',
              symbolSize: 7,
              lineStyle: { width: 0, opacity: 0 },
              // Hollow: the plot's own colour inside, the series' outline.
              itemStyle: { color: p.surface1, borderColor: colorOf(s, i), borderWidth: 1.5, opacity: 0.85 },
              yAxisIndex: s.axis === 1 ? 1 : 0,
            },
          ]
        : [],
    );

    return {
      animation: false,
      textStyle: { fontFamily: p.sans },
      grid: { left: 52, right: hasRight ? 52 : 16, top: 14, bottom: showZoom ? 44 : 24, containLabel: false },
      tooltip: {
        trigger: 'axis',
        confine: true,
        // Pinned to the plot's top-right corner: the same place every time.
        position: (_pt: unknown, _params: unknown, _dom: unknown, _rect: unknown, size: { contentSize: number[]; viewSize: number[] }) => [
          size.viewSize[0] - size.contentSize[0] - (hasRight ? 56 : 20),
          10,
        ],
        axisPointer: { type: 'line', lineStyle: { color: p.text, width: 1, opacity: 0.7, type: 'solid' } },
        backgroundColor: p.surface3,
        borderColor: p.lineStrong,
        padding: [6, 10],
        textStyle: { color: p.text, fontSize: 11, fontFamily: p.sans },
        extraCssText: `box-shadow: none; border-radius: 6px; font-variant-numeric: tabular-nums;`,
        // The tooltip is where absolute time actually pays off: it is what an
        // operator copies into a relay log search.
        formatter: (params: unknown) => {
          const rows = Array.isArray(params) ? params : [params];
          const first = rows[0] as { value?: [number, number | null] } | undefined;
          const at = first?.value?.[0];
          const head = typeof at === 'number' ? tooltipTime(at) : '';
          const body = rows
            .map((r) => {
              const q = r as { marker?: string; seriesName?: string; value?: [number, number | null] };
              const v = q.value?.[1];
              // An absent reading renders as an em dash, never as 0.
              return `${q.marker ?? ''}${escapeHtml(q.seriesName ?? '')} <b>${v === null || v === undefined ? '—' : formatValue(v)}</b>`;
            })
            .join('<br/>');
          return `${head}<br/>${body}`;
        },
      },
      xAxis: {
        type: 'time',
        min: fromMs,
        max: toMs,
        ...ax,
        axisLabel: { ...ax.axisLabel, formatter: axisTime, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: [
        {
          type: 'value',
          name: unit,
          nameTextStyle: { color: p.text3, fontSize: 10, align: 'left' },
          nameGap: 6,
          ...ax,
          // A fixed label width is what stops the plot area — and therefore the
          // whole row — from shifting as a value crosses 9.9 → 10.
          axisLabel: { ...ax.axisLabel, width: 40, overflow: 'truncate' },
          scale: false,
          min: 0,
        },
        ...(hasRight
          ? [
              {
                type: 'value' as const,
                name: unitRight,
                nameTextStyle: { color: p.text3, fontSize: 10 },
                ...ax,
                splitLine: { show: false },
              },
            ]
          : []),
      ],
      // The slider sits BELOW the axis labels: R31's overprinted them.
      dataZoom: showZoom
        ? [
            { id: ZOOM_ID, type: 'inside', filterMode: 'none' },
            {
              type: 'slider',
              height: 16,
              bottom: 4,
              filterMode: 'none',
              showDetail: false,
              showDataShadow: false,
              borderColor: p.lineStrong,
              backgroundColor: p.bg,
              fillerColor: p.hatch,
              handleStyle: { color: p.surface3, borderColor: p.text2 },
              moveHandleStyle: { color: p.surface3 },
            },
          ]
        : [{ id: ZOOM_ID, type: 'inside', filterMode: 'none' }],
      series: [...lines, ...hollows],
    } as EChartsOption;
    // colorOf closes over p only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [p, series, bands, markers, fromMs, toMs, unit, unitRight, showZoom]);

  const ref = useChart(option, { group });
  const showLegend = legend ?? series.length > 1;

  return (
    <figure className={styles.wrap}>
      {showLegend && option !== null && (
        <div className={styles.legend}>
          {series.map((s, i) => (
            <span key={s.key}>
              <i style={{ background: colorOf(s, i) }} />
              {s.label}
            </span>
          ))}
        </div>
      )}
      {/* The container keeps its height whether or not there is data, so an
          empty state and a populated one occupy the same space. */}
      <div className={styles.plot} style={{ height }}>
        <div className={styles.canvas} ref={ref} role="img" aria-label={ariaLabel} />
        {option === null && <p className={styles.empty}>{empty ?? 'No data in this range.'}</p>}
        {option !== null && overlay ? <div className={styles.overlay}>{overlay}</div> : null}
      </div>
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

// Series names come from the data (a SQL column, a trend group): ECharts sets
// the formatter's output as HTML, so they are escaped.
function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}
