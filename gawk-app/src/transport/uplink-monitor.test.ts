// "Your upload can't keep up" (docs/75 RU9, §6), on the native monitor's rule
// exactly (gawk-broadcast-desktop/crates/engine/src/uplink.rs): one sample a
// second; a second is bad on any stale drop, on two or more keyframe streams
// superseded or failed, or on a sent rate under 75 % of the encode rate above
// 20 fps; five bad seconds raise the notice, fifteen good ones clear it.

import { describe, expect, it } from 'vitest';

import {
  UPLINK_CLEAR_AFTER_GOOD_SECONDS,
  UPLINK_WARN_AFTER_BAD_SECONDS,
  UplinkMonitor,
  type UplinkSample,
} from './uplink-monitor';
import { R84_LAB_SECONDS } from './uplink-monitor.r84-fixture';

const healthy = (superseded = 0): UplinkSample => ({
  staleDropped: 0,
  keyframeStreamsSuperseded: superseded,
  keyframeStreamsFailed: 0,
  encoderFps: 60,
  sentFps: 58,
});

describe('UplinkMonitor (RU9, the engine rule)', () => {
  it('pins the engine thresholds', () => {
    expect(UPLINK_WARN_AFTER_BAD_SECONDS).toBe(5);
    expect(UPLINK_CLEAR_AFTER_GOOD_SECONDS).toBe(15);
  });

  it('a healthy stream never warns, even with the odd superseded keyframe', () => {
    const m = new UplinkMonitor();
    for (let i = 0; i < 60; i++) expect(m.observe(healthy(Math.floor(i / 30)))).toBe(false);
  });

  it('any stale drop, video or audio, makes a bad second; five raise the notice', () => {
    const m = new UplinkMonitor();
    const st = healthy();
    m.observe({ ...st });
    for (let i = 1; i <= 5; i++) {
      st.staleDropped += 1;
      expect(m.observe({ ...st }), `second ${i}`).toBe(i >= 5);
    }
  });

  it('two keyframe streams superseded or failed in a second is bad; one is not', () => {
    const m = new UplinkMonitor();
    const st = healthy();
    m.observe({ ...st });
    for (let i = 0; i < 10; i++) {
      st.keyframeStreamsSuperseded += 1;
      expect(m.observe({ ...st })).toBe(false);
    }
    const m2 = new UplinkMonitor();
    m2.observe({ ...st });
    let warned = false;
    for (let i = 0; i < 5; i++) {
      st.keyframeStreamsSuperseded += 1;
      st.keyframeStreamsFailed += 1;
      warned = m2.observe({ ...st });
    }
    expect(warned).toBe(true);
  });

  it('a sent rate under 75 % of the encode rate is bad above 20 fps only', () => {
    const starved = new UplinkMonitor();
    let warned = false;
    for (let i = 0; i < 6; i++) warned = starved.observe({ ...healthy(), encoderFps: 60, sentFps: 44 });
    expect(warned).toBe(true);

    const slow = new UplinkMonitor();
    for (let i = 0; i < 10; i++) expect(slow.observe({ ...healthy(), encoderFps: 20, sentFps: 5 })).toBe(false);

    const edge = new UplinkMonitor();
    for (let i = 0; i < 10; i++) expect(edge.observe({ ...healthy(), encoderFps: 60, sentFps: 45 })).toBe(false);
  });

  it('clears only after fifteen good seconds, and a bad second restarts the run', () => {
    const m = new UplinkMonitor();
    const st = healthy();
    m.observe({ ...st });
    for (let i = 0; i < 5; i++) {
      st.staleDropped += 3;
      m.observe({ ...st });
    }
    for (let i = 1; i < 14; i++) expect(m.observe({ ...st }), `clean second ${i}`).toBe(true);
    st.staleDropped += 1; // bad again
    expect(m.observe({ ...st })).toBe(true);
    for (let i = 1; i < 15; i++) expect(m.observe({ ...st }), `clean second ${i}`).toBe(true);
    expect(m.observe({ ...st })).toBe(false);
  });
});

// docs/88 D5, G5: in Reliable mode the notice reads the carrier lane's counts
// — a second is bad on any shed record, on two or more keyframe streams
// superseded or failed, or on a sent rate under 75 % of the encode rate —
// replayed here second by second from docs/88's lab runs of the inline carrier
// on Chrome 151 (uplink-monitor.r84-fixture.ts). Quiet wherever the carrier
// carries the stream (clean at 10–100 ms, light loss, bursts and outages at
// 10 ms, outages at 40 ms); raised where Chrome's Cubic collapses (40 ms with
// 0.5 % loss, 100 ms with 1 %) and under 40 ms bursts, where records were shed
// for 58 of the run's seconds.
describe('UplinkMonitor in Reliable mode, on the lab counters (docs/88 D5)', () => {
  function replay(rows: readonly (readonly [number, number, number, number, number])[]) {
    const m = new UplinkMonitor();
    let shed = 0;
    let superseded = 0;
    let failed = 0;
    let firstWarnAt: number | null = null;
    let warnedSeconds = 0;
    rows.forEach(([encoded, sent, recordsShed, sup, fail], i) => {
      shed += recordsShed;
      superseded += sup;
      failed += fail;
      const warned = m.observe({
        staleDropped: shed,
        keyframeStreamsSuperseded: superseded,
        keyframeStreamsFailed: failed,
        encoderFps: encoded,
        sentFps: sent,
      });
      if (warned) {
        warnedSeconds++;
        firstWarnAt ??= i;
      }
    });
    return { firstWarnAt, warnedSeconds };
  }

  it.each(['rtt10-clean', 'rtt40-clean', 'rtt100-clean', 'rtt10-loss0.005', 'rtt10-ge1pct', 'rtt10-out80drop', 'rtt40-out80drop'])(
    '%s: never raised',
    (cell) => {
      expect(replay(R84_LAB_SECONDS[cell]).firstWarnAt).toBeNull();
    },
  );

  it.each([
    ['rtt40-loss0.005', 13, 87],
    ['rtt100-loss0.01', 9, 92],
    ['rtt40-ge1pct', 32, 58],
  ])('%s: raised at second %i, up for %i seconds', (cell, first, seconds) => {
    expect(replay(R84_LAB_SECONDS[cell])).toEqual({ firstWarnAt: first, warnedSeconds: seconds });
  });
});
