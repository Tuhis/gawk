// UD11: Apache ECharts, BUNDLED, with explicit component registration.
//
// Chosen over visx, uPlot and Recharts/Nivo for one load-bearing reason that is
// not about looks: **`echarts.connect()` synchronises crosshair and zoom across
// separate chart instances**, which IS TH4's multi-lane broadcast timeline —
// built in rather than built by us. Canvas rendering makes 5 lanes × 2 000
// points a non-event; `markArea`/`markLine` give hidden-tab shading, dip
// episodes and event marks natively.
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
//
// R36 (docs/86 TU3) moved every colour out of this file: they come from the
// theme's tokens through `palette.ts`, so a light page gets light charts.

import * as echarts from 'echarts/core';
import { LineChart } from 'echarts/charts';
import {
  DataZoomComponent,
  GridComponent,
  MarkAreaComponent,
  MarkLineComponent,
  TooltipComponent,
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

import type { Palette } from './palette.ts';

// EXACTLY what is used, and nothing kept "in case":
//
//   LineChart      every series on every surface (thin buckets are a line
//                  series with hollow symbols and no stroke)
//   Grid/Tooltip   the plot area and the crosshair (the tooltip's content is
//                  hidden on stacks, where the readout rail carries values)
//   DataZoom       inside-drag zoom, and the slider under a stack
//   MarkArea       hidden-tab and not-joined veils, dip episodes
//   MarkLine       re-homes, notes, and the floor ticks of stored events
//   CanvasRenderer 5 lanes x 2 000 points is a non-event on canvas
//
// Legends are HTML now (they carry the value at the crosshair), so the
// LegendComponent is no longer registered.
//
// TH7's fleet stripes are deliberately NOT a chart: they are one span per
// broadcast positioned by percentage, which makes the vertical alignment exact
// and costs two elements per row instead of an instance.
echarts.use([
  LineChart,
  GridComponent,
  TooltipComponent,
  DataZoomComponent,
  MarkAreaComponent,
  MarkLineComponent,
  CanvasRenderer,
]);

export { echarts };
export type EChartsType = echarts.ECharts;
export type EChartsOption = echarts.EChartsCoreOption;

/**
 * The id every chart's zoom component carries. `echarts.connect()` relays a
 * zoom as an action naming its component, so a stack only zooms as one when
 * every member's zoom has the SAME id — the slider under a stack included.
 */
export const ZOOM_ID = 'zoom';

/** Shared axis styling, so every chart on the page reads as one system. */
export function axisStyle(p: Palette) {
  return {
    axisLine: { lineStyle: { color: p.lineStrong } },
    axisTick: { show: false },
    axisLabel: { color: p.text3, fontSize: 10, fontFamily: p.sans },
    splitLine: { lineStyle: { color: p.line } },
  };
}

/**
 * A diagonal hatch, as a canvas pattern ECharts can fill an area with: the
 * "was not here / could not see" veil (not joined, left, tab hidden). Hatched
 * rather than tinted, so it can never read as a severity.
 */
const hatchCache = new Map<string, HTMLCanvasElement>();
export function hatchPattern(color: string): { image: HTMLCanvasElement; repeat: 'repeat' } | string {
  let c = hatchCache.get(color);
  if (!c) {
    try {
      c = document.createElement('canvas');
      c.width = 7;
      c.height = 7;
      const g = c.getContext('2d');
      if (!g) return color;
      g.strokeStyle = color;
      g.lineWidth = 2;
      g.beginPath();
      g.moveTo(-1, 8);
      g.lineTo(8, -1);
      g.stroke();
      hatchCache.set(color, c);
    } catch {
      return color;
    }
  }
  return { image: c, repeat: 'repeat' };
}

