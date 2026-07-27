import { describe, expect, it } from 'vitest';

import type { Snapshot } from '../api/types.ts';
import {
  coverageMs,
  ingest,
  MAX_POINTS,
  RETAIN_AFTER_GONE_MS,
  series,
  WINDOW_MS,
  type HistoryMap,
} from './history.ts';

function snap(atMs: number, metrics: Record<string, number>, sessionId = 's1'): Snapshot {
  return {
    atMs,
    live: [
      {
        broadcastKey: 'b1',
        lifecycle: 'live',
        severity: 'ok',
        worstViewer: 'ok',
        viewers: 1,
        uptimeMs: 1000,
        sessions: [
          {
            sessionId,
            broadcastKey: 'b1',
            role: 'viewer',
            severity: 'ok',
            clientAgeMs: 0,
            relayAgeMs: 0,
            clientState: 'reporting',
            relayState: 'observed',
            metrics,
          },
        ],
      },
    ],
    ended: null,
  };
}

describe('history', () => {
  it('accumulates one point per snapshot', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(1000, { receivedFps: 30 }), 1000);
    h = ingest(h, snap(3000, { receivedFps: 28 }), 3000);
    expect(h.s1.points.map((p) => p.v.receivedFps)).toEqual([30, 28]);
  });

  // A manual refresh, or two polls racing, must not stack duplicate points on
  // one timestamp — that would put a vertical step in a graph where the data
  // has none.
  it('replaces rather than duplicates a repeated timestamp', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(1000, { receivedFps: 30 }), 1000);
    h = ingest(h, snap(1000, { receivedFps: 31 }), 1100);
    expect(h.s1.points).toHaveLength(1);
    expect(h.s1.points[0].v.receivedFps).toBe(31);
  });

  it('drops points older than the window', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(0, { receivedFps: 30 }), 0);
    const later = WINDOW_MS + 5000;
    h = ingest(h, snap(later, { receivedFps: 25 }), later);
    expect(h.s1.points).toHaveLength(1);
    expect(h.s1.points[0].v.receivedFps).toBe(25);
  });

  // The window is enforced by timestamp, but a clock jump must not be able to
  // grow a buffer without bound.
  it('caps points regardless of timestamps', () => {
    let h: HistoryMap = {};
    for (let i = 0; i < MAX_POINTS + 50; i++) {
      h = ingest(h, snap(i, { receivedFps: i }), 1);
    }
    expect(h.s1.points.length).toBeLessThanOrEqual(MAX_POINTS);
  });

  it('forgets a session once it has been gone long enough', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(1000, { receivedFps: 30 }), 1000);
    // A later snapshot without that session at all.
    const gone: Snapshot = { atMs: 2000, live: [], ended: null };
    h = ingest(h, gone, 1000 + RETAIN_AFTER_GONE_MS - 1);
    expect(h.s1).toBeDefined(); // still inside the retention grace
    h = ingest(h, gone, 1000 + RETAIN_AFTER_GONE_MS + 1);
    expect(h.s1).toBeUndefined();
  });

  // An absent metric and a zero are different claims. `series` must drop the
  // absent ones so the chart BREAKS the line rather than drawing through a gap
  // as though the value had glided across it.
  it('excludes points where a metric was absent, without inventing zeroes', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(1000, { receivedFps: 30 }), 1000);
    h = ingest(h, snap(2000, {}), 2000);
    h = ingest(h, snap(3000, { receivedFps: 10 }), 3000);
    expect(series(h.s1, 'receivedFps').map((p) => p.v.receivedFps)).toEqual([30, 10]);
  });

  it('reports how much wall-clock it actually covers', () => {
    let h: HistoryMap = {};
    expect(coverageMs(h.s1)).toBe(0);
    h = ingest(h, snap(1000, { receivedFps: 30 }), 1000);
    expect(coverageMs(h.s1)).toBe(0); // one point covers nothing
    h = ingest(h, snap(61000, { receivedFps: 30 }), 61000);
    expect(coverageMs(h.s1)).toBe(60000);
  });

  it('keeps each session separate', () => {
    let h: HistoryMap = {};
    h = ingest(h, snap(1000, { receivedFps: 30 }, 'a'), 1000);
    h = ingest(h, snap(2000, { receivedFps: 10 }, 'b'), 2000);
    expect(Object.keys(h).sort()).toEqual(['a', 'b']);
  });
});
