import { beforeEach, afterEach, describe, expect, it } from 'vitest';
import { getRuntimeConfig, requiresPublishSecret,
  getDvrBufferMs,
  DEFAULT_DVR_BUFFER_MS,
  MIN_DVR_BUFFER_MS,
  MAX_DVR_BUFFER_MS,
} from './config';

beforeEach(() => {
  globalThis.window = {
    location: {
      hostname: 'localhost',
    },
  } as any;
});

afterEach(() => {
  delete (globalThis as any).window;
});

describe('runtime config', () => {
  it('defaults to an empty config when nothing is injected, defaulting to true in dev', () => {
    expect(getRuntimeConfig()).toEqual({});
    expect(requiresPublishSecret()).toBe(true);
  });

  it('reads requirePublishSecret from the injected global', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: true };
    expect(requiresPublishSecret()).toBe(true);
  });

  it('honors an explicit false flag even in dev', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: false };
    expect(requiresPublishSecret()).toBe(false);
  });

  it('defaults to true in dev if the config object is present but flag is missing', () => {
    window.__GAWK_CONFIG__ = {};
    expect(requiresPublishSecret()).toBe(true);
  });
});

// R21 (docs/26): the Deep buffer floor is a deploy-time knob, because DV6 sets
// it by measurement and re-tuning must not need an image rebuild.
describe('getDvrBufferMs', () => {
  afterEach(() => {
    delete window.__GAWK_CONFIG__;
  });

  it('defaults when unset', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getDvrBufferMs()).toBe(DEFAULT_DVR_BUFFER_MS);
  });

  it('takes a configured value', () => {
    window.__GAWK_CONFIG__ = { dvrBufferMs: 5000 };
    expect(getDvrBufferMs()).toBe(5000);
  });

  it('clamps values a relay could not honour', () => {
    // Below the relay's minimum it would be silently downgraded to plain
    // resilient delivery; in the minutes it is a config error, not a choice.
    window.__GAWK_CONFIG__ = { dvrBufferMs: 10 };
    expect(getDvrBufferMs()).toBe(MIN_DVR_BUFFER_MS);
    window.__GAWK_CONFIG__ = { dvrBufferMs: 600_000 };
    expect(getDvrBufferMs()).toBe(MAX_DVR_BUFFER_MS);
  });

  it('ignores nonsense rather than breaking the viewer', () => {
    window.__GAWK_CONFIG__ = { dvrBufferMs: Number.NaN };
    expect(getDvrBufferMs()).toBe(DEFAULT_DVR_BUFFER_MS);
    window.__GAWK_CONFIG__ = { dvrBufferMs: 'lots' as unknown as number };
    expect(getDvrBufferMs()).toBe(DEFAULT_DVR_BUFFER_MS);
  });
});
