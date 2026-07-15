import { describe, expect, it } from 'vitest';

import {
  FRAMERATE_RUNGS,
  FRAMERATE_SELECTIONS,
  RESOLUTION_RUNGS,
  RESOLUTION_SELECTIONS,
  applyCeiling,
  autoLadder,
  clampBitrateOverride,
  computeBitrate,
  computeTargetSize,
  hardwareCeiling,
  resolveAutoFps,
  rungCapWidth,
  type ResolutionRung,
  type SupportLookup,
} from './ladder';

// Lookup factory: hardware for combos the predicate approves, software
// otherwise (the probe matrix's all-software degradation shape).
function lookupWhere(hw: (rung: ResolutionRung, fps: number) => boolean): SupportLookup {
  return (rung, fps) => ({ acceleration: hw(rung, fps) ? 'hardware' : 'software' });
}

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

describe('RESOLUTION_SELECTIONS', () => {
  it('leads with auto as the default, followed by the explicit rungs', () => {
    expect(RESOLUTION_SELECTIONS[0]).toBe('auto');
    expect(RESOLUTION_SELECTIONS).toEqual(['auto', ...RESOLUTION_RUNGS]);
  });
});

describe('autoLadder', () => {
  it('yields the full ladder for a 4K source', () => {
    expect(autoLadder(3840)).toEqual(['native', 1080, 720, 480]);
  });

  it('skips the no-op 1080 rung for a 1080p source (its 1920 cap would not shrink it)', () => {
    expect(autoLadder(1920)).toEqual(['native', 720, 480]);
  });

  it('skips 1080 and 720 for a 720p source', () => {
    expect(autoLadder(1280)).toEqual(['native', 480]);
  });

  it('yields only native for a source at the 480 cap', () => {
    expect(autoLadder(854)).toEqual(['native']);
  });

  it('yields only native for a sub-480p source', () => {
    expect(autoLadder(640)).toEqual(['native']);
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

describe('FRAMERATE_SELECTIONS (R12)', () => {
  it('leads with auto as the default, followed by the explicit rungs', () => {
    expect(FRAMERATE_SELECTIONS[0]).toBe('auto');
    expect(FRAMERATE_SELECTIONS).toEqual(['auto', ...FRAMERATE_RUNGS]);
  });
});

describe('resolveAutoFps (docs/17 Decision 4)', () => {
  it('resolves framerate-first: 60 when any rung probes hardware at 60', () => {
    // Only 720p does HW at 60 — fps still wins over resolution.
    expect(resolveAutoFps(lookupWhere((rung, fps) => rung === 720 && fps === 60))).toBe(60);
  });

  it('falls back to 30 when nothing probes hardware at 60', () => {
    expect(resolveAutoFps(lookupWhere((_rung, fps) => fps === 30))).toBe(30);
  });

  it('an all-software matrix (Firefox shape) resolves 30 — never native', () => {
    expect(resolveAutoFps(lookupWhere(() => false))).toBe(30);
  });
});

describe('hardwareCeiling (docs/17 Decision 3)', () => {
  it('picks the highest rung that probes hardware at the effective fps', () => {
    expect(hardwareCeiling(lookupWhere((rung) => rung !== 'native'), 60)).toBe(1080);
    expect(hardwareCeiling(lookupWhere((rung) => rung === 720 || rung === 480), 60)).toBe(720);
  });

  it('native wins when it probes hardware', () => {
    expect(hardwareCeiling(lookupWhere(() => true), 60)).toBe('native');
  });

  it('an all-software matrix yields the 1080p software ceiling', () => {
    expect(hardwareCeiling(lookupWhere(() => false), 60)).toBe(1080);
  });
});

describe('applyCeiling', () => {
  it('passes the full ladder through a native ceiling', () => {
    expect(applyCeiling(autoLadder(3840), 'native', 3840)).toEqual(['native', 1080, 720, 480]);
  });

  it('slices a 4K ladder at a 1080p ceiling (native excluded — 4K exceeds the cap)', () => {
    expect(applyCeiling(autoLadder(3840), 1080, 3840)).toEqual([1080, 720, 480]);
  });

  it('keeps native for a source already under the ceiling cap (720p source, 1080p ceiling)', () => {
    // The ceiling bounds pixel count, not the rung label: native === 1280 wide here.
    expect(applyCeiling(autoLadder(1280), 1080, 1280)).toEqual(['native', 480]);
  });

  it('never returns empty', () => {
    expect(applyCeiling(['native'], 480, 3840)).toEqual(['native']);
  });
});

describe('rungCapWidth', () => {
  it('maps rungs to their longer-dimension caps, null for native', () => {
    expect(rungCapWidth('native')).toBeNull();
    expect(rungCapWidth(1080)).toBe(1920);
    expect(rungCapWidth(720)).toBe(1280);
    expect(rungCapWidth(480)).toBe(854);
  });
});

describe('clampBitrateOverride (docs/17 Decision 11)', () => {
  it('clamps to the [0.5, 50] Mbps override band', () => {
    expect(clampBitrateOverride(1)).toBe(500_000);
    expect(clampBitrateOverride(80_000_000)).toBe(50_000_000);
    expect(clampBitrateOverride(12_000_000)).toBe(12_000_000);
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
