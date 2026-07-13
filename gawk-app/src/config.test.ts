// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest';
import { getRuntimeConfig, requiresPublishSecret } from './config';

afterEach(() => {
  delete window.__GAWK_CONFIG__;
});

describe('runtime config', () => {
  it('defaults to an empty config when nothing is injected', () => {
    expect(getRuntimeConfig()).toEqual({});
    expect(requiresPublishSecret()).toBe(false);
  });

  it('reads requirePublishSecret from the injected global', () => {
    window.__GAWK_CONFIG__ = { requirePublishSecret: true };
    expect(requiresPublishSecret()).toBe(true);
  });

  it('treats a missing/false flag as not required', () => {
    window.__GAWK_CONFIG__ = {};
    expect(requiresPublishSecret()).toBe(false);
    window.__GAWK_CONFIG__ = { requirePublishSecret: false };
    expect(requiresPublishSecret()).toBe(false);
  });
});
