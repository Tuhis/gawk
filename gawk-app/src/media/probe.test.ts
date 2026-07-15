// R13 (docs/18) L1: the probe matrix classifies each (rung, fps) combo as
// hardware / software / unsupported from isConfigSupported answers, honors
// the acceleration policy, memoizes, and never throws.

import { describe, expect, it, vi } from 'vitest';

import {
  DEFAULT_PROBE_SOURCE,
  EncoderSupportProber,
  probeDims,
  probeSupportMatrix,
  type IsConfigSupportedFn,
} from './probe';

const CODECS = ['avc1.4D4034', 'vp8'];

// A stub browser: decides support per probe call. `hw` / `sw` are
// predicates over (codec, width, framerate).
function stubIsSupported(opts: {
  hw?: (config: VideoEncoderConfig) => boolean;
  sw?: (config: VideoEncoderConfig) => boolean;
}): { fn: IsConfigSupportedFn; calls: VideoEncoderConfig[] } {
  const calls: VideoEncoderConfig[] = [];
  const fn: IsConfigSupportedFn = (config) => {
    calls.push(config);
    const supported =
      config.hardwareAcceleration === 'prefer-hardware'
        ? (opts.hw?.(config) ?? false)
        : (opts.sw?.(config) ?? false);
    return Promise.resolve({ supported, config });
  };
  return { fn, calls };
}

describe('probeDims', () => {
  it('uses the rung target when it shrinks the source', () => {
    expect(probeDims(1080, DEFAULT_PROBE_SOURCE)).toEqual({ width: 1920, height: 1080 });
  });

  it('falls back to source dims for native and non-shrinking rungs', () => {
    expect(probeDims('native', DEFAULT_PROBE_SOURCE)).toEqual(DEFAULT_PROBE_SOURCE);
    expect(probeDims(1080, { width: 1280, height: 720 })).toEqual({ width: 1280, height: 720 });
  });
});

describe('probeSupportMatrix classification', () => {
  it('classifies hardware, software and unsupported per (rung, fps)', async () => {
    // HW up to 1080p@60; everything else software except 5 fps unsupported.
    const { fn } = stubIsSupported({
      hw: (c) => c.width! <= 1920,
      sw: (c) => c.framerate !== 5,
    });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
    });
    expect(matrix.get(1080, 60)).toEqual({ acceleration: 'hardware', codec: 'avc1.4D4034' });
    expect(matrix.get('native', 60)).toEqual({ acceleration: 'software', codec: 'avc1.4D4034' });
    expect(matrix.get('native', 5)).toEqual({ acceleration: 'unsupported', codec: null });
  });

  it('a Firefox-shaped browser (all prefer-hardware rejected) yields an all-software matrix', async () => {
    const { fn } = stubIsSupported({ hw: () => false, sw: () => true });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
    });
    for (const entry of matrix.entries.values()) {
      expect(entry.acceleration).toBe('software');
    }
  });

  it('treats a prefer-hardware answer resolved to prefer-software as not hardware', async () => {
    const fn: IsConfigSupportedFn = (config) =>
      Promise.resolve({
        supported: true,
        config: { ...config, hardwareAcceleration: 'prefer-software' as HardwareAcceleration },
      });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
    });
    expect(matrix.get('native', 60).acceleration).toBe('software');
  });

  it('unknown combos read as unsupported', async () => {
    const { fn } = stubIsSupported({ hw: () => true });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
    });
    expect(matrix.get(1080, 144)).toEqual({ acceleration: 'unsupported', codec: null });
  });
});

describe('codec preference order', () => {
  it('the first codec that resolves wins', async () => {
    const { fn } = stubIsSupported({ hw: (c) => c.codec === 'vp8' });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
    });
    expect(matrix.get('native', 60)).toEqual({ acceleration: 'hardware', codec: 'vp8' });
  });

  it('a codec pin (single-entry list) narrows the walk to that codec only', async () => {
    const { fn, calls } = stubIsSupported({ hw: () => true });
    await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: ['vp8'],
      hwPreference: 'auto',
      fpsValues: [60],
    });
    expect(calls.length).toBeGreaterThan(0);
    for (const call of calls) expect(call.codec).toBe('vp8');
  });
});

describe('acceleration policy', () => {
  it('hardware mode classifies not-HW combos as unsupported (no software pass)', async () => {
    const { fn, calls } = stubIsSupported({ hw: (c) => c.width! <= 1920, sw: () => true });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'hardware',
      fpsValues: [60],
    });
    expect(matrix.get(1080, 60).acceleration).toBe('hardware');
    expect(matrix.get('native', 60).acceleration).toBe('unsupported');
    for (const call of calls) expect(call.hardwareAcceleration).toBe('prefer-hardware');
  });

  it('software mode probes prefer-software only and never reports hardware', async () => {
    const { fn, calls } = stubIsSupported({
      hw: () => true,
      sw: (c) => c.hardwareAcceleration === 'prefer-software',
    });
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'software',
      fpsValues: [60],
    });
    expect(matrix.get('native', 60)).toEqual({ acceleration: 'software', codec: 'avc1.4D4034' });
    for (const call of calls) expect(call.hardwareAcceleration).toBe('prefer-software');
  });
});

describe('robustness', () => {
  it('probe exceptions classify as unsupported and never escape', async () => {
    const fn: IsConfigSupportedFn = () => Promise.reject(new Error('boom'));
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
    });
    expect(matrix.get('native', 60)).toEqual({ acceleration: 'unsupported', codec: null });
  });

  it('a synchronously-throwing probe fn is also contained', async () => {
    const fn: IsConfigSupportedFn = () => {
      throw new Error('sync boom');
    };
    const matrix = await probeSupportMatrix(new EncoderSupportProber(fn), {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
    });
    expect(matrix.get('native', 60).acceleration).toBe('unsupported');
  });
});

describe('memoization + native refinement', () => {
  it('memoizes identical combos across matrix runs', async () => {
    const { fn, calls } = stubIsSupported({ hw: () => true });
    const spy = vi.fn(fn);
    const prober = new EncoderSupportProber(spy);
    const opts = { codecs: CODECS, hwPreference: 'auto' as const, fpsValues: [60] };
    await probeSupportMatrix(prober, opts);
    const callsAfterFirst = spy.mock.calls.length;
    await probeSupportMatrix(prober, opts);
    expect(spy.mock.calls.length).toBe(callsAfterFirst);
    expect(calls.length).toBe(callsAfterFirst);
  });

  it('re-probes the native rung when refined source dims arrive', async () => {
    const { fn } = stubIsSupported({ hw: () => true });
    const spy = vi.fn(fn);
    const prober = new EncoderSupportProber(spy);
    await probeSupportMatrix(prober, { codecs: CODECS, hwPreference: 'auto', fpsValues: [60] });
    const before = spy.mock.calls.length;
    // Real capture turns out to be ultrawide 3440x1440.
    const refined = await probeSupportMatrix(prober, {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
      source: { width: 3440, height: 1440 },
    });
    expect(spy.mock.calls.length).toBeGreaterThan(before);
    expect(spy.mock.calls.slice(before).some(([c]) => c.width === 3440)).toBe(true);
    expect(refined.source).toEqual({ width: 3440, height: 1440 });
  });

  it('non-shrinking rungs share the native combo (no duplicate probes)', async () => {
    const { fn } = stubIsSupported({ hw: () => true });
    const spy = vi.fn(fn);
    // A 720p source: native, 1080 and 720 all probe at 1280x720.
    await probeSupportMatrix(new EncoderSupportProber(spy), {
      codecs: CODECS,
      hwPreference: 'auto',
      fpsValues: [60],
      source: { width: 1280, height: 720 },
    });
    const dims = new Set(spy.mock.calls.map(([c]) => `${c.width}x${c.height}`));
    expect(dims).toEqual(new Set(['1280x720', '854x480']));
  });
});
