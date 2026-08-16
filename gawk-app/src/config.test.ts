import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
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
  getRelayUrl,
  getDevCertHashHex,
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

  // The one value a self-hosted install cannot do without: without it the
  // app falls back to https://localhost:4433 on every origin but the
  // reference deployment's, and each viewer has to paste the URL by hand.
  it('reports the configured relay URL, and empty when unset', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getRelayUrl()).toBe('');
    window.__GAWK_CONFIG__ = { relayUrl: 'https://relay.example.com:4433' };
    expect(getRelayUrl()).toBe('https://relay.example.com:4433');
  });

  // The ConfigMap renders "" rather than omitting the key, so blank and
  // whitespace must read as unset like every other getter here.
  it('treats a blank relay URL as unset', () => {
    window.__GAWK_CONFIG__ = { relayUrl: '   ' };
    expect(getRelayUrl()).toBe('');
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

// R38 (docs/41 D4): the local stack's dev certificate hash. The gate is the
// point of the key — it exists so `#/view/{id}` works on a local stack, and
// must be inert anywhere else.
describe('getDevCertHashHex', () => {
  afterEach(() => {
    delete window.__GAWK_CONFIG__;
  });

  it('returns the configured hash on a loopback origin', () => {
    window.__GAWK_CONFIG__ = { devCertHashHex: 'a1b2c3' };
    expect(getDevCertHashHex()).toBe('a1b2c3');
  });

  // isDevEnvironment() is "Vite dev mode OR a loopback hostname", and the test
  // runner is itself Vite dev mode — so proving the hostname half of the gate
  // means turning the other half off.
  it('returns "" on a non-loopback origin even when the key is set', () => {
    vi.stubEnv('DEV', false);
    (window as any).location = { hostname: 'gawk.example.com' };
    window.__GAWK_CONFIG__ = { devCertHashHex: 'a1b2c3' };
    expect(getDevCertHashHex()).toBe('');
    vi.unstubAllEnvs();
  });

  it('treats empty and whitespace as unset', () => {
    window.__GAWK_CONFIG__ = {};
    expect(getDevCertHashHex()).toBe('');
    window.__GAWK_CONFIG__ = { devCertHashHex: '' };
    expect(getDevCertHashHex()).toBe('');
    window.__GAWK_CONFIG__ = { devCertHashHex: '   ' };
    expect(getDevCertHashHex()).toBe('');
  });
});

// D4 says this key is deliberately NOT a chart value: offering the knob would
// invite a production deployment to paper over a TLS misconfiguration with
// it. A deliberate omission is invisible to a reader of the chart, so it is
// asserted here rather than left to be "tidied up" by a later sweep.
describe('the gawk-app chart does not learn devCertHashHex (D4)', () => {
  // Read through Vite's glob rather than node:fs — this package has no Node
  // type definitions and D4 is not worth a dependency.
  const chartFiles = import.meta.glob('../deploy/charts/**/*', {
    query: '?raw',
    import: 'default',
    eager: true,
  }) as Record<string, string>;

  it('sees the chart at all (a vacuous pass would prove nothing)', () => {
    expect(Object.keys(chartFiles).length).toBeGreaterThan(0);
    expect(Object.keys(chartFiles).some((p) => p.endsWith('configmap.yaml'))).toBe(true);
  });

  it('is absent from every file under deploy/charts', () => {
    const offenders = Object.entries(chartFiles)
      .filter(([, body]) => body.includes('devCertHashHex'))
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });
});
