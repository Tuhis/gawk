// @vitest-environment jsdom
import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { TimelineChart, type SeriesSpec } from './TimelineChart.tsx';
import type { Point } from '../lib/history.ts';

// RTL only auto-cleans when vitest globals are on; this project uses per-file
// `@vitest-environment` docblocks instead, so the DOM would otherwise
// accumulate across cases and every query would match the previous render too.
afterEach(cleanup);

const SERIES: SeriesSpec[] = [{ key: 'renderedFps', label: 'rendered', color: 'red' }];
const WINDOW = 600_000;

function points(spec: Array<{ fps?: number; hidden?: boolean }>): Point[] {
  return spec.map((s, i) => ({
    t: i * 2000,
    v: {
      ...(s.fps === undefined ? {} : { renderedFps: s.fps }),
      ...(s.hidden === undefined ? {} : { documentHidden: s.hidden ? 1 : 0 }),
    },
  }));
}

describe('TimelineChart', () => {
  it('draws one path per series with data', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30 }, { fps: 28 }, { fps: 30 }])}
        series={SERIES}
        windowMs={WINDOW}
      />,
    );
    const paths = container.querySelectorAll('path');
    expect(paths).toHaveLength(1);
    expect(paths[0].getAttribute('d')).toContain('M');
  });

  // A metric that stopped being reported did not glide to its next value.
  // Drawing through the gap would invent data, so each contiguous run is its
  // own subpath — which shows up as a second `M` command.
  it('breaks the line at a gap instead of interpolating across it', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30 }, {}, { fps: 30 }])}
        series={SERIES}
        windowMs={WINDOW}
      />,
    );
    const d = container.querySelector('path')?.getAttribute('d') ?? '';
    expect(d.match(/M/g)).toHaveLength(2);
  });

  // A hidden tab stops firing rAF, so the rendered line collapses for a reason
  // that has nothing to do with the stream. The shaded span is what says which.
  it('shades a contiguous hidden span', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([
          { fps: 30, hidden: false },
          { fps: 0, hidden: true },
          { fps: 0, hidden: true },
          { fps: 30, hidden: false },
        ])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
        shadeLabel="tab backgrounded"
      />,
    );
    const rects = container.querySelectorAll('rect');
    expect(rects).toHaveLength(1);
    expect(Number(rects[0].getAttribute('width'))).toBeGreaterThan(0);
  });

  it('shades two separate spans separately', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([
          { fps: 30, hidden: false },
          { fps: 0, hidden: true },
          { fps: 30, hidden: false },
          { fps: 0, hidden: true },
          { fps: 30, hidden: false },
        ])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
      />,
    );
    expect(container.querySelectorAll('rect')).toHaveLength(2);
  });

  // A span still open at the right-hand edge is the live case — the tab is
  // hidden right now — and must be shaded to the edge rather than dropped for
  // want of a closing point.
  it('shades a span that is still open at the end', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30, hidden: false }, { fps: 0, hidden: true }, { fps: 0, hidden: true }])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
      />,
    );
    expect(container.querySelectorAll('rect')).toHaveLength(1);
  });

  it('shades nothing when the tab was never hidden', () => {
    const { container } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30, hidden: false }, { fps: 30, hidden: false }])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
      />,
    );
    expect(container.querySelectorAll('rect')).toHaveLength(0);
  });

  // The meaning has to survive a greyscale screenshot, so the shading is
  // labelled in words rather than carried by the tint alone.
  it('labels the shading in the legend when a span exists', () => {
    const { container, queryByText } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30, hidden: false }, { fps: 0, hidden: true }])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
        shadeLabel="tab backgrounded"
      />,
    );
    expect(container.querySelectorAll('rect').length).toBeGreaterThan(0);
    expect(queryByText('tab backgrounded')).toBeTruthy();
  });

  it('says nothing about shading when there is none to explain', () => {
    const { queryByText } = render(
      <TimelineChart
        title="Frame rate"
        unit="fps"
        points={points([{ fps: 30, hidden: false }])}
        series={SERIES}
        windowMs={WINDOW}
        shadeKey="documentHidden"
        shadeLabel="tab backgrounded"
      />,
    );
    expect(queryByText('tab backgrounded')).toBeNull();
  });
});
