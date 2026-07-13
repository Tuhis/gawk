import { beforeEach, afterEach, describe, expect, it } from 'vitest';
import { getRuntimeConfig, requiresPublishSecret } from './config';

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
