// R15 N5 (docs/20 Decision 10): A/V sync — skew measurement on synthetic
// clocks, the audio-master display-target mapping, its slew limit, and the
// arrival-baseline fallback when audio goes absent or stale.

import { beforeEach, describe, expect, it } from 'vitest';

import {
  audioClockAvailable,
  audioDisplayTargetMs,
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

  it('derives display targets from the playhead: a frame ahead waits that long', () => {
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    // Frame 80 ms ahead of the playhead, 150 ms playout offset ⇒ display at
    // now + 80 + 150.
    expect(audioDisplayTargetMs(1_080_000, 150, 0)).toBeCloseTo(230, 3);
    // A frame at the playhead waits only the offset.
    expect(audioDisplayTargetMs(1_000_000, 150, 0)).toBeCloseTo(150, 3);
  });

  it('falls back (null) when audio is absent or the clock goes stale', () => {
    // No reports at all.
    expect(audioDisplayTargetMs(1_000_000, 150, 0)).toBeNull();

    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioDisplayTargetMs(1_000_000, 150, 0)).not.toBeNull();

    // Reports stop: after the staleness bound the audio clock is not a
    // master any more and the caller uses the arrival baseline.
    expect(audioClockAvailable(1400)).toBe(true);
    expect(audioClockAvailable(2000)).toBe(false);
    expect(audioDisplayTargetMs(1_000_000, 150, 2000)).toBeNull();
    expect(observeVideoPresented(1_000_000, 2000)).toBeNull();
    expect(getAvSkewMs()).toBeNull();
  });

  // The Decision 10 requirement: switching the target source must never
  // step video. A re-anchored audio clock is absorbed gradually.
  it('slew-limits a re-anchored audio clock instead of stepping targets', () => {
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    const before = audioDisplayTargetMs(1_000_000, 0, 250)!;

    // The sink re-anchors 500 ms forward (a flush + fresh timeline).
    notePlayhead({ playheadUs: 1_750_000, atEpochMs: epochFor(250) }, 250);
    const after = audioDisplayTargetMs(1_000_000, 0, 250)!;

    // Without slew limiting this target would jump by the full 500 ms.
    const jump = Math.abs(after - before);
    expect(jump).toBeLessThan(30);
    expect(jump).toBeGreaterThan(0);
  });

  it('reset clears the mapping (restart: the old anchor is a dead timeline)', () => {
    notePlayhead({ playheadUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(true);
    resetAvSync();
    expect(audioClockAvailable(0)).toBe(false);
    expect(getAvSkewMs()).toBeNull();
    expect(audioDisplayTargetMs(1_000_000, 150, 0)).toBeNull();
  });
});

// The live-edge guarantee (docs/20 Decision 10): the audio clock exists and
// is measured in every mode, but only the paced modes consult it for display
// targets. This pins the *seam* — displayTargetMs is undefined outside
// adaptive mode, so no audio state can delay video there.
describe('av-sync does not bend the live-edge default', () => {
  it('skew is measured even when the audio clock never drives targets', () => {
    notePlayhead({ playheadUs: 500_000, atEpochMs: epochFor(0) }, 0);
    // Measurement is unconditional…
    expect(observeVideoPresented(560_000, 0)).toBeCloseTo(60, 3);
    // …and asking for a target is the caller's separate, mode-gated choice.
    expect(audioDisplayTargetMs(560_000, 0, 0)).not.toBeNull();
  });
});
