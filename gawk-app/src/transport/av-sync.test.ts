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
