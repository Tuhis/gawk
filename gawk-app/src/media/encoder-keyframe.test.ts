import { describe, expect, it } from 'vitest';

import { KeyframeCadence } from './encoder';

const US = 1_000_000;

// Feed `seconds` of frames at `fps`, return the indices that were keyframes.
function run(cadence: KeyframeCadence, fps: number, seconds: number): number[] {
  const keyIndices: number[] = [];
  const count = fps * seconds;
  for (let i = 0; i < count; i++) {
    const ts = Math.round((i * US) / fps);
    if (cadence.shouldKeyframe(ts)) keyIndices.push(i);
  }
  return keyIndices;
}

describe('KeyframeCadence', () => {
  it('makes the first frame a keyframe', () => {
    const cadence = new KeyframeCadence(2000);
    expect(cadence.shouldKeyframe(0)).toBe(true);
  });

  it('spaces keyframes ~every 2s at 60 fps (matching the old 120-frame cadence)', () => {
    const keys = run(new KeyframeCadence(2000), 60, 10);
    expect(keys[0]).toBe(0);
    expect(keys).toHaveLength(5); // 10s / 2s
    expect(keys[1]).toBe(120);
  });

  it('spaces keyframes ~every 2s at 5 fps (every 10 frames, NOT every 120)', () => {
    const keys = run(new KeyframeCadence(2000), 5, 10);
    expect(keys).toHaveLength(5);
    expect(keys[1]).toBe(10);
  });

  it('keys immediately after a gap longer than the interval', () => {
    const cadence = new KeyframeCadence(2000);
    expect(cadence.shouldKeyframe(0)).toBe(true);
    expect(cadence.shouldKeyframe(1 * US)).toBe(false);
    // 10s stall — the next frame must be a keyframe.
    expect(cadence.shouldKeyframe(11 * US)).toBe(true);
  });
  // R17 W2: forceNext() makes exactly the next frame a keyframe, regardless
  // of cadence, then normal spacing resumes from it (auto-resume re-attach).
  it('forceNext keys the very next frame once, then resumes the cadence', () => {
    const cadence = new KeyframeCadence(2000);
    expect(cadence.shouldKeyframe(0)).toBe(true);
    expect(cadence.shouldKeyframe(0.5 * US)).toBe(false);
    cadence.forceNext();
    expect(cadence.shouldKeyframe(1 * US)).toBe(true); // forced, mid-GOP
    expect(cadence.shouldKeyframe(1.5 * US)).toBe(false); // one-shot
    expect(cadence.shouldKeyframe(3 * US)).toBe(true); // 2s after the forced key
  });
});
