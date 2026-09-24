// Browser support detection.
//
// The detector is feature-only. The one thing worth pinning beyond "no API →
// warn" is the absence of an engine check: a user-agent check must not creep
// back in without a live, browser-specific defect to justify it.

import { describe, expect, it } from 'vitest';
import { detectBrowserSupport } from './browserSupport';

describe('detectBrowserSupport', () => {
  it('flags a browser with no WebTransport at all', () => {
    const r = detectBrowserSupport({ hasWebTransport: false });
    expect(r.supported).toBe(false);
    expect(r.supported === false && r.reason).toBe('no-webtransport');
    expect(r.supported === false && r.browserLabel).toBe('This browser');
  });

  it('treats any browser that has WebTransport as supported', () => {
    expect(detectBrowserSupport({ hasWebTransport: true })).toEqual({ supported: true });
  });
});
