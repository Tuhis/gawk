// A/V sync (docs/20 Decision 10, revised 2026-07-20 to video-master): skew
// measurement on synthetic clocks, the staleness bound, and the drift trim
// that is the only lever left once playback has started.

import { beforeEach, describe, expect, it } from 'vitest';

import {
  AudioRateController,
  RATE_TRIM_DEADBAND_MS,
  RATE_TRIM_GIVE_UP_MS,
  RATE_TRIM_MAX,
  audioClockAvailable,
  getAvSkewMs,
  notePlayhead,
  observeVideoPresented,
  resetAvSync,
} from './av-sync';

// The module carries the reporting context's absolute epoch; in tests both
// contexts share performance.timeOrigin, so epoch = timeOrigin + localMs.
function epochFor(localMs: number): number {
  return performance.timeOrigin + localMs;
}

beforeEach(() => resetAvSync());

describe('av-sync playhead mapping', () => {
  it('has no audio clock until a playhead with real audio arrives', () => {
    expect(audioClockAvailable(0)).toBe(false);
    // Audio exists but nothing has played yet: still no clock.
    notePlayhead({ playheadUs: null, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(false);

    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(true);
  });

  it('computes skew as video − audio on the shared broadcaster clock', () => {
    // At local t=0 the speaker is playing broadcaster timestamp 1.000 s.
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);

    // A frame stamped 1.050 s presented right now: video is 50 ms ahead.
    expect(observeVideoPresented(1_050_000, 0)).toBeCloseTo(50, 3);
    expect(getAvSkewMs()).toBeCloseTo(50, 3);

    // A frame stamped 0.980 s: video is 20 ms behind.
    expect(observeVideoPresented(980_000, 0)).toBeCloseTo(-20, 3);

    // 100 ms later the playhead has advanced by 100 ms too, so the same
    // frame timestamp reads 100 ms more behind.
    expect(observeVideoPresented(1_050_000, 100)).toBeCloseTo(-50, 3);
  });

  it('perfect pacing holds skew at ~0 as both clocks advance', () => {
    notePlayhead({ playheadUs: 0, atEpochMs: epochFor(0) }, 0);
    for (let t = 0; t <= 2000; t += 250) {
      notePlayhead({ playheadUs: t * 1000, atEpochMs: epochFor(t) }, t);
      const skew = observeVideoPresented(t * 1000, t);
      expect(Math.abs(skew!)).toBeLessThan(1);
    }
  });

  it('stops being a clock once reports go stale', () => {
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(1400)).toBe(true);
    expect(audioClockAvailable(2000)).toBe(false);
    expect(observeVideoPresented(1_000_000, 2000)).toBeNull();
    expect(getAvSkewMs()).toBeNull();
  });

  it('reset clears the mapping (restart: the old anchor is a dead timeline)', () => {
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(true);
    resetAvSync();
    expect(audioClockAvailable(0)).toBe(false);
    expect(getAvSkewMs()).toBeNull();
  });
});

// The mapping's 20 ms/s slew smooths drift, but a re-anchor (the audio jitter
// buffer under-runs, re-primes, and resumes at the live edge) moves the
// playhead discontinuously by hundreds of ms to seconds. Slewing that back at
// 20 ms/s left the skew reading ~2 s and creeping for the ~100 s it took to
// reconverge (the field capture: 1939→2130 over 9.5 s = exactly 20 ms/s). A
// large discrepancy is not drift; the mapping snaps to it in one report.
describe('av-sync snaps on a re-anchor instead of crawling at the slew cap', () => {
  it('tracks a 2 s playhead jump in one report (was ~2000 ms of slew lag)', () => {
    // Converge: at local 5000 ms the speaker is playing broadcaster ts 5.000 s.
    for (let t = 0; t <= 5000; t += 250) {
      notePlayhead({ playheadUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    expect(observeVideoPresented(5_000_000, 5000)!).toBeCloseTo(0, 0);

    // The audio buffer re-primes after a deep stall and resumes at the live
    // edge: the next report's playhead is 2 s ahead of where the mapping was.
    const t = 5250;
    const jumped = (t + 2000) * 1000;
    notePlayhead({ playheadUs: jumped, atEpochMs: epochFor(t) }, t);

    // A frame at the new live edge reads ~0 (snapped). Pre-fix the mapping
    // corrected only 5 ms of the 2 s gap, so it read ~1995 ms.
    expect(Math.abs(observeVideoPresented(jumped, t)!)).toBeLessThan(50);
  });

  it('does not let a frozen playhead ramp the skew at the slew rate', () => {
    for (let t = 0; t <= 5000; t += 250) {
      notePlayhead({ playheadUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    // The worklet under-runs: its playhead freezes while the wall clock (and
    // the audio that eventually resumes) keep moving. Pre-fix, each stale
    // report dragged the mapping down 5 ms and the skew climbed 20 ms/s.
    const frozen = 5000 * 1000;
    for (let t = 5250; t <= 8000; t += 250) {
      notePlayhead({ playheadUs: frozen, atEpochMs: epochFor(t) }, t);
    }
    // Audio resumes at the live edge; the very next report must re-align.
    const t = 8250;
    const resumed = (t - 30) * 1000; // 30 ms behind live, healthy
    notePlayhead({ playheadUs: resumed, atEpochMs: epochFor(t) }, t);
    expect(observeVideoPresented(t * 1000, t)!).toBeCloseTo(30, 0);
  });

  it('still slews through ordinary report-arrival jitter (no spurious snap)', () => {
    notePlayhead({ playheadUs: 0, atEpochMs: epochFor(0) }, 0);
    // Reports land with ±40 ms arrival jitter while the playhead advances at
    // 1×: well under the re-anchor threshold, so the slew smooths it.
    for (let k = 1; k <= 40; k++) {
      const arrive = k * 250 + (k % 2 === 0 ? 40 : -40);
      notePlayhead({ playheadUs: k * 250 * 1000, atEpochMs: epochFor(arrive) }, arrive);
    }
    const nowMs = 40 * 250;
    // A frame at the true current playhead reads within jitter of 0 — smoothed,
    // not snapped to a noisy instantaneous sample.
    expect(Math.abs(observeVideoPresented(nowMs * 1000, nowMs)!)).toBeLessThan(60);
  });
});

// The video-master guarantee (docs/20 Decision 10 revised, field finding 4):
// av-sync measures, and nothing more. It exports no way to reschedule video,
// so no audio state — fresh, stale, or absent — can move a video frame. That
// is the property the revision buys, and this pins it at the module surface.
describe('av-sync cannot reschedule video', () => {
  it('measures skew without exposing any video-side lever', async () => {
    notePlayhead({ playheadUs: 500_000, atEpochMs: epochFor(0) }, 0);
    expect(observeVideoPresented(560_000, 0)).toBeCloseTo(60, 3);

    const surface = Object.keys(await import('./av-sync'));
    expect(surface).not.toContain('audioDisplayTargetMs');
    expect(surface).not.toContain('audioBaselineMs');
  });
});

// Drift is what remains after the start-time alignment (field finding 4):
// the worklet runs at 1×, so nothing else can move audio relative to video.
describe('AudioRateController', () => {
  const ctl = () => new AudioRateController();

  it('leaves small skew alone rather than modulating pitch for nothing', () => {
    const c = ctl();
    for (let t = 0; t <= 20_000; t += 250) c.update(RATE_TRIM_DEADBAND_MS - 5, t);
    expect(c.current()).toBe(1);
  });

  it('speeds up when audio is behind and slows when ahead, within the audible bound', () => {
    const behind = ctl();
    for (let t = 0; t <= 60_000; t += 250) behind.update(600, t);
    expect(behind.current()).toBeGreaterThan(1);
    expect(behind.current()).toBeLessThanOrEqual(1 + RATE_TRIM_MAX);

    const ahead = ctl();
    for (let t = 0; t <= 60_000; t += 250) ahead.update(-600, t);
    expect(ahead.current()).toBeLessThan(1);
    expect(ahead.current()).toBeGreaterThanOrEqual(1 - RATE_TRIM_MAX);
  });

  // The user requirement in one assertion: corrections are spread over a long
  // period. A step would be audible as a pitch jump.
  it('never steps — the rate itself is slew-limited', () => {
    const c = ctl();
    let prev = 1;
    for (let t = 250; t <= 10_000; t += 250) {
      const next = c.update(2000 - 1, t);
      expect(Math.abs(next - prev)).toBeLessThan(0.0005);
      prev = next;
    }
  });

  it('gives up on a skew too large to be drift, and returns toward 1x', () => {
    const c = ctl();
    for (let t = 0; t <= 60_000; t += 250) c.update(500, t);
    expect(c.current()).toBeGreaterThan(1);
    // A re-anchor jump: audio-buffer's flush owns this, not a rate grind.
    for (let t = 60_000; t <= 120_000; t += 250) c.update(RATE_TRIM_GIVE_UP_MS + 1, t);
    expect(c.current()).toBeCloseTo(1, 5);
  });

  it('holds at 1x while skew is unmeasurable', () => {
    const c = ctl();
    for (let t = 0; t <= 10_000; t += 250) c.update(null, t);
    expect(c.current()).toBe(1);
  });
});
