import { useEffect, useRef } from 'react';

import { echarts, type EChartsOption, type EChartsType } from './echarts.ts';

// UD11's second constraint: **a small local hook, not `echarts-for-react`.**
// One dependency instead of two, and the lifecycle is ours.
//
// The lifecycle is the whole reason this file exists. ECharts is imperative and
// long-lived inside a declarative tree, which is the classic source of leaked
// instances and stale closures on hot reload (docs/36 §7's named risk). Three
// rules hold it together:
//
//   * **One instance per mounted element, disposed on unmount.** Nothing else
//     may create one.
//   * **Options are SET, never merged blindly.** `notMerge` is true by
//     default: a chart whose series list shrank must lose the old series, and
//     ECharts' default merge keeps them forever.
//   * **`connect` groups are joined on mount and left on unmount**, so a lane
//     that disappears cannot keep governing the crosshair of the ones that
//     remain.

export interface ChartOptions {
  /**
   * A group id. Charts sharing one are joined by `echarts.connect()`, which is
   * what makes TH4's lanes a single crosshair and a single zoom rather than N
   * charts that happen to be stacked.
   */
  group?: string;
  /** Passed to setOption. Defaults to true; see the note above. */
  notMerge?: boolean;
  onInit?: (chart: EChartsType) => void;
}

/**
 * Mount an ECharts instance on a div and keep its option in sync.
 *
 * Returns the ref to attach. The instance is never exposed as state: a render
 * triggered by "the chart exists now" is a render nobody asked for, and the
 * imperative handle is only useful inside effects anyway.
 */
export function useChart(
  option: EChartsOption | null,
  opts: ChartOptions = {},
): React.RefObject<HTMLDivElement | null> {
  const ref = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<EChartsType | null>(null);
  const { group, notMerge = true, onInit } = opts;

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const chart = echarts.init(el, undefined, { renderer: 'canvas' });
    chartRef.current = chart;
    onInit?.(chart);

    // The container is sized by CSS, and ECharts cannot observe that on its
    // own. Without this a chart in a flex column renders at its first measured
    // size forever and then lies about its axis after any layout change.
    const ro = new ResizeObserver(() => chart.resize());
    ro.observe(el);

    return () => {
      ro.disconnect();
      chartRef.current = null;
      chart.dispose();
    };
    // `onInit` is intentionally not a dependency: re-creating the instance
    // because a callback identity changed is exactly the leak this hook exists
    // to prevent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart || !option) return;
    chart.setOption(option, { notMerge });
  }, [option, notMerge]);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart || !group) return;
    chart.group = group;
    echarts.connect(group);
    return () => {
      // Leaving the group on unmount matters more than joining it: a disposed
      // instance left in a connect group makes the SURVIVING charts throw on
      // the next crosshair move.
      chart.group = '';
    };
  }, [group]);

  return ref;
}
