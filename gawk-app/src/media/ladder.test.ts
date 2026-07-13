import { describe, expect, it } from 'vitest';

import {
  FRAMERATE_RUNGS,
  RESOLUTION_RUNGS,
  computeBitrate,
  computeTargetSize,
} from './ladder';

describe('RESOLUTION_RUNGS / FRAMERATE_RUNGS', () => {
  it('lead with native as the default', () => {
    expect(RESOLUTION_RUNGS[0]).toBe('native');
    expect(FRAMERATE_RUNGS[0]).toBe('native');
  });

  it('cover the ladder from the design doc', () => {
    expect(RESOLUTION_RUNGS).toEqual(['native', 1080, 720, 480]);
    expect(FRAMERATE_RUNGS).toEqual(['native', 60, 30, 5]);
  });
});

describe('computeTargetSize', () => {
  it('returns null (passthrough) for the native rung', () => {
    expect(computeTargetSize(3840, 2160, 'native')).toBeNull();
  });

  it('scales 4K down to 1920x1080 at the 1080p rung', () => {
    expect(computeTargetSize(3840, 2160, 1080)).toEqual({ width: 1920, height: 1080 });
  });

  it('scales 1080p to 1280x720 at the 720p rung', () => {
    expect(computeTargetSize(1920, 1080, 720)).toEqual({ width: 1280, height: 720 });
  });

  it('caps the longer dimension for ultrawide sources', () => {
    // 3440x1440 at the 1080p rung: width capped at 1920, AR preserved.
    expect(computeTargetSize(3440, 1440, 1080)).toEqual({ width: 1920, height: 804 });
  });

  it('bounds pathological aspect ratios: 25000x1080 is NOT "already 1080p"', () => {
    // Shorter-dimension capping would pass this 27-megapixel frame through.
    const target = computeTargetSize(25000, 1080, 1080);
    expect(target).toEqual({ width: 1920, height: 82 });
  });

  it('caps the longer dimension (height) for portrait sources', () => {
    expect(computeTargetSize(1440, 2560, 720)).toEqual({ width: 720, height: 1280 });
  });

  it('passes through a portrait source whose longer dimension is within the cap', () => {
    expect(computeTargetSize(1080, 1920, 1080)).toBeNull();
  });

  it('never upscales: source at the rung passes through', () => {
    expect(computeTargetSize(1920, 1080, 1080)).toBeNull();
  });

  it('never upscales: source below the rung passes through', () => {
    expect(computeTargetSize(1280, 720, 1080)).toBeNull();
  });

  it('rounds scaled dimensions down to even (H.264 requirement)', () => {
    // 3442x1440 at 1080p: scale 1920/3442 -> 803.3 tall; must come out even.
    const target = computeTargetSize(3442, 1440, 1080);
    expect(target).not.toBeNull();
    expect(target!.width % 2).toBe(0);
    expect(target!.height % 2).toBe(0);
    expect(target!.width).toBe(1920);
  });

  it('produces even dimensions for 1080p -> 480p (non-integral scale)', () => {
    const target = computeTargetSize(1920, 1080, 480);
    expect(target).toEqual({ width: 854, height: 480 });
  });

  it('never produces a zero dimension for degenerate sources', () => {
    const target = computeTargetSize(20000, 2, 1080);
    expect(target).not.toBeNull();
    expect(target!.height).toBeGreaterThanOrEqual(2);
    expect(target!.width % 2).toBe(0);
  });
});

describe('computeBitrate', () => {
  it('anchors 1080p60 near the historical fixed 6 Mbps', () => {
    expect(computeBitrate(1920, 1080, 60)).toBe(6_220_800);
  });

  it('scales with pixel count: 720p60', () => {
    expect(computeBitrate(1280, 720, 60)).toBe(2_764_800);
  });

  it('scales sub-linearly with framerate (sqrt), keeping low-fps streams crisp', () => {
    const at60 = computeBitrate(1920, 1080, 60);
    const at5 = computeBitrate(1920, 1080, 5);
    // sqrt(5/60) ~ 0.2887 — well above the linear 1/12.
    expect(at5).toBeGreaterThan(at60 / 12);
    expect(at5).toBeLessThan(at60 / 3);
  });

  it('clamps to the 10 Mbps ceiling for native 4K', () => {
    expect(computeBitrate(3840, 2160, 60)).toBe(10_000_000);
  });

  it('clamps to the 0.5 Mbps floor for tiny low-fps output', () => {
    expect(computeBitrate(852, 480, 5)).toBe(500_000);
  });
});
