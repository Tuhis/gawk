import { beforeEach, afterEach, describe, expect, it } from 'vitest';
import { getRuntimeConfig, requiresPublishSecret,
  getDvrBufferMs,
  DEFAULT_DVR_BUFFER_MS,
  MIN_DVR_BUFFER_MS,
  MAX_DVR_BUFFER_MS,
  BUNDLED_TERMS_VERSION,
  getTermsVersion,
  getOperatorName,
  getOperatorContact,
  getTermsUrl,
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

// R23 (docs/29): terms metadata accessors. Empty/whitespace strings count as
// unset so the ConfigMap can render empty defaults without duplicating the
// version constant or printing blank attribution.
describe('terms config', () => {
  afterEach(() => {
    delete window.__GAWK_CONFIG__;
  });

  it('falls back to the bundled version when unset or blank', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getTermsVersion()).toBe(BUNDLED_TERMS_VERSION);
    window.__GAWK_CONFIG__ = { termsVersion: '   ' };
    expect(getTermsVersion()).toBe(BUNDLED_TERMS_VERSION);
  });

  it('takes a configured version', () => {
    window.__GAWK_CONFIG__ = { termsVersion: '2027-01-01' };
    expect(getTermsVersion()).toBe('2027-01-01');
  });

  it('shows a neutral operator name when unset', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getOperatorName()).toBe('the operator of this deployment');
    window.__GAWK_CONFIG__ = { operatorName: 'Juho’s homelab' };
    expect(getOperatorName()).toBe('Juho’s homelab');
  });

  it('returns null contact/url when unset (so the UI degrades, no blank/fetch)', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getOperatorContact()).toBeNull();
    expect(getTermsUrl()).toBeNull();
    window.__GAWK_CONFIG__ = { operatorContact: 'x@y.z', termsUrl: '/terms.html' };
    expect(getOperatorContact()).toBe('x@y.z');
    expect(getTermsUrl()).toBe('/terms.html');
  });
});
