// UD11: Apache ECharts, BUNDLED, with explicit component registration.
//
// Chosen over visx, uPlot and Recharts/Nivo for one load-bearing reason that is
// not about looks: **`echarts.connect()` synchronises crosshair and zoom across
// separate chart instances**, which IS TH4's multi-lane broadcast timeline and
// TH7's fleet timeline — built in rather than built by us. Canvas rendering
// makes 5 lanes × 2 000 points a non-event; `markArea`/`markLine` give
// hidden-tab shading, dip episodes and event markers natively.
//
// Two constraints follow from the decision and are enforced here:
//
//   1. **Import from `echarts/core` with explicit registration, never the
//      barrel.** The whole-package import is several times the size, and
//      tree-shaking is ours to control on a page that must ship inside a Go
//      binary.
//   2. **Nothing is fetched at runtime.** UD7 is not a preference: the page
//      must work on a port-forward from a laptop with no network, and
//      `internal/dashboard/dashboard_test.go` asserts it over the built
//      output. That test is the one to run deliberately after touching this
//      file — and it SKIPS unless `npm run build` has been run first, so a
//      green `go test ./internal/dashboard/` on its own proves nothing.

import * as echarts from 'echarts/core';
import { LineChart } from 'echarts/charts';
import {
  DataZoomComponent,
  GridComponent,
  LegendComponent,
  MarkAreaComponent,
  MarkLineComponent,
  TooltipComponent,
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

// EXACTLY what is used, and nothing kept "in case". Every entry below is
// reachable from TimeChart:
//
//   LineChart      every series on every surface
//   Grid/Tooltip   the plot area and the crosshair readout
//   Legend         multi-series charts (the explorer, trends)
//   DataZoom       inside-drag zoom, and the slider on the last chart of a page
//   MarkArea       hidden-tab shading, dip episodes, join/leave spans
//   MarkLine       stored events, annotations, relay re-homes
//   CanvasRenderer 5 lanes x 2 000 points is a non-event on canvas
//
// TH7's fleet stripes are deliberately NOT a chart: they are one span div per
// broadcast positioned by percentage, which makes the vertical alignment exact
// and costs two elements per row instead of an instance.
echarts.use([
  LineChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  DataZoomComponent,
  MarkAreaComponent,
  MarkLineComponent,
  CanvasRenderer,
]);

export { echarts };
export type EChartsType = echarts.ECharts;
export type EChartsOption = echarts.EChartsCoreOption;

/**
 * The chart palette.
 *
 * Hue stays reserved for SEVERITY (UD14) — so series colours are deliberately
 * neutral-leaning and are never green/amber/red. A line that happens to be red
 * because it is the third series would collide with the one channel this page
 * uses to mean "broken".
 */
export const SERIES_COLORS = [
  '#58a6ff',
  '#a371f7',
  '#39c5cf',
  '#d2a8ff',
  '#79c0ff',
  '#6e7681',
];

export const SEVERITY_COLORS: Record<string, string> = {
  ok: '#3fb950',
  warn: '#e3a83c',
  bad: '#e5534b',
  // `unknown` gets NO hue, so "checked, healthy" and "nothing ever reported"
  // can never be confused — the page's cardinal rule, in the chart layer.
  unknown: '#6b7280',
};

/** Shared axis/grid styling, so every chart on the page reads as one system. */
export const AXIS_STYLE = {
  axisLine: { lineStyle: { color: '#333846' } },
  axisTick: { show: false },
  axisLabel: { color: '#9aa0ab', fontSize: 10 },
  splitLine: { lineStyle: { color: 'rgba(38,42,50,0.6)' } },
};

export const GRID = { left: 52, right: 16, top: 18, bottom: 24, containLabel: false };

/** Severity -> the translucent band colour used for a degraded span. */
export function severityBand(sev: string): string {
  const base = SEVERITY_COLORS[sev] ?? SEVERITY_COLORS.unknown;
  const n = parseInt(base.slice(1), 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, 0.22)`;
}
