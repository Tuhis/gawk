// Browser support detection. Behavior is written first (CODE-REVIEW.md).
//
// The detector is feature-only. The one thing worth pinning beyond "no API →
// warn" is the absence of an engine check: WebKit was warned about by user
// agent for six weeks while the relay refused it (docs/gotchas.md, the
// webtransport-go `Server.Config` entry), and that check must not creep back
// in without a live, browser-specific defect to justify it.

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
