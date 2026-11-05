import { useMemo } from 'react';

import type { EChartsOption, EChartsType } from './echarts.ts';
import { usePalette } from './palette.ts';
import {
  buildStackOption,
  emptyStackOption,
  type StackBand,
  type StackMark,
  type StackSeries,
  type StackZoom,
} from './stackOption.ts';
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
  /** The stack's current zoom, read whenever the option is rebuilt. */
  zoom?: () => StackZoom;
}

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
  zoom,
}: Props) {
  const p = usePalette();
  const hasData = series.some((s) => s.data.some(([, v]) => v !== null));

  const option = useMemo<EChartsOption | null>(
    () => {
      // Read, not depended on: a zoom is applied by the stack's relay, and
      // only a rebuild for some OTHER reason needs to carry it.
      const z = zoom?.();
      return hasData
        ? buildStackOption(p, { series, bands, marks, ticks, fromMs, toMs, yLabels, quiet }, z)
        : emptyStackOption(p, fromMs, toMs, z);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
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

