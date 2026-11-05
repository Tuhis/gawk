import { useMemo } from 'react';

import { ZOOM_ID, type EChartsOption, type EChartsType } from './echarts.ts';
import { usePalette } from './palette.ts';
import { cursorLabel, timeTicks } from './stack.ts';
import { useChart } from './useChart.ts';
import styles from './TimeAxis.module.css';

/**
 * A stack's one time axis (R36): ticks for the visible window and the
 * crosshair's instant as a pill. HTML, not a chart, so it can sit sticky above
 * the lanes and cost no canvas.
 */
export function TimeAxis({
  viewFrom,
  viewTo,
  cursorMs,
  max = 8,
}: {
  viewFrom: number;
  viewTo: number;
  cursorMs: number | null;
  max?: number;
}) {
  const ticks = useMemo(() => timeTicks(viewFrom, viewTo, max), [viewFrom, viewTo, max]);
  const span = viewTo - viewFrom;
  const cursorPct = cursorMs !== null && span > 0 ? ((cursorMs - viewFrom) / span) * 100 : null;
  return (
    <div className={styles.ticks} aria-hidden>
      {ticks.map((t) => (
        <span
          key={t.atMs}
          className={styles.tick}
          // Keep the outermost labels inside the plot rather than half-clipped.
          style={{ left: `${t.pct}%`, transform: `translateX(${t.pct < 4 ? '0' : t.pct > 96 ? '-100%' : '-50%'})` }}
        >
          {t.label}
        </span>
      ))}
      {cursorPct !== null && cursorPct >= 0 && cursorPct <= 100 && (
        <span
          className={styles.cursor}
          style={{
            left: `${cursorPct}%`,
            transform: `translateX(${cursorPct < 6 ? '0' : cursorPct > 94 ? '-100%' : '-50%'})`,
          }}
        >
          {cursorLabel(cursorMs!)}
        </span>
      )}
    </div>
  );
}

/**
 * The zoom slider under a stack: a chart with nothing in it but the slider,
 * joined to the stack's connect group so dragging it zooms every lane. Its
 * zoom carries the same id as theirs (`ZOOM_ID`), which is what makes the
 * relayed action land.
 */
export function ZoomBar({
  fromMs,
  toMs,
  group,
  onInit,
  zoom,
}: {
  fromMs: number;
  toMs: number;
  group: string;
  onInit?: (chart: EChartsType) => void;
  /** The stack's current zoom, so a rebuild (a theme switch) keeps it. */
  zoom?: () => { start: number; end: number };
}) {
  const p = usePalette();
  const option = useMemo<EChartsOption>(
    () => {
      const z = zoom?.() ?? { start: 0, end: 100 };
      return {
        animation: false,
        grid: { left: 0, right: 0, top: 0, bottom: 0 },
        xAxis: { type: 'time', min: fromMs, max: toMs, show: false },
        yAxis: { type: 'value', show: false },
        series: [{ type: 'line', data: [], showSymbol: false }],
        dataZoom: [
          {
            id: ZOOM_ID,
            type: 'slider',
            filterMode: 'none',
            start: z.start,
            end: z.end,
            left: 8,
            right: 8,
            top: 2,
            bottom: 2,
            showDetail: false,
            showDataShadow: false,
            brushSelect: false,
            borderColor: p.lineStrong,
            backgroundColor: p.bg,
            fillerColor: p.hatch,
            borderRadius: 4,
            handleStyle: { color: p.surface3, borderColor: p.text2 },
            moveHandleStyle: { color: p.surface3, opacity: 0.6 },
            emphasis: { handleStyle: { color: p.text2 }, moveHandleStyle: { color: p.text3 } },
            dataBackground: { lineStyle: { opacity: 0 }, areaStyle: { opacity: 0 } },
          },
        ],
      } as EChartsOption;
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [p, fromMs, toMs],
  );
  const ref = useChart(option, { group, onInit });
  return <div className={styles.zoom} ref={ref} role="slider" aria-label="Zoom window" aria-valuetext="drag to zoom every lane" />;
}
