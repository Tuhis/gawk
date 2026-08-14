// Browser support detection. Behavior is written first (CODE-REVIEW.md).
//
// The stakes are asymmetric and that shapes every case below: a missed WebKit
// client sees a "streamer offline" card for a defect that is ours, while a
// false positive puts a scary modal in front of a browser that works fine.
// The macOS Chrome / Android Chrome cases exist to pin the false-positive side,
// because both carry "Safari" in their UA and one of them reports touch points.

import { describe, expect, it } from 'vitest';
import { detectBrowserSupport, type BrowserEnv } from './browserSupport';

const env = (userAgent: string, over: Partial<BrowserEnv> = {}): BrowserEnv => ({
  userAgent,
  maxTouchPoints: 0,
  hasWebTransport: true,
  ...over,
});

// Real strings, kept verbatim — a paraphrased UA proves nothing.
const UA = {
  macSafari:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15',
  iphoneSafari:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1',
  // iPadOS 13+ claims to be a Mac; maxTouchPoints is the only discriminator.
  ipadSafari:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15',
  chromeIOS:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/119.0.6045.109 Mobile/15E148 Safari/604.1',
  firefoxIOS:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/121.0 Mobile/15E148 Safari/605.1.15',
  edgeIOS:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) EdgiOS/120.0.0.0 Mobile/15E148 Safari/605.1.15',
  macChrome:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
  macFirefox: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:121.0) Gecko/20100101 Firefox/121.0',
  macEdge:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0',
  winChrome:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
  linuxFirefox: 'Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0',
  androidChrome:
    'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36',
  // Headless Chrome says "HeadlessChrome/141", never "Chrome/141" — the same
  // trap telemetry.ts's browserClass documents. On macOS that leaves a UA with
  // "Macintosh" and "Safari/" and no recognizable Chromium brand.
  macHeadlessChrome:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/141.0.0.0 Safari/537.36',
};

describe('detectBrowserSupport', () => {
  it('flags macOS Safari as an unsupported WebKit engine', () => {
    const r = detectBrowserSupport(env(UA.macSafari));
    expect(r.supported).toBe(false);
    expect(r.supported === false && r.reason).toBe('webkit');
    expect(r.supported === false && r.browserLabel).toBe('Safari');
  });

  it('flags iPhone Safari', () => {
    const r = detectBrowserSupport(env(UA.iphoneSafari));
    expect(r.supported === false && r.reason).toBe('webkit');
    expect(r.supported === false && r.browserLabel).toBe('Safari on iOS');
  });

  it('flags iPadOS Safari despite its Macintosh user agent', () => {
    const r = detectBrowserSupport(env(UA.ipadSafari, { maxTouchPoints: 5 }));
    expect(r.supported === false && r.reason).toBe('webkit');
    expect(r.supported === false && r.browserLabel).toBe('Safari on iOS');
  });

  // Every iOS browser is WebKit underneath (App Store policy), so all of them
  // fail identically. Naming the actual browser matters: a Chrome-on-iPhone
  // user told "Safari is unsupported" would reasonably think it didn't apply.
  it.each([
    ['Chrome on iOS', UA.chromeIOS],
    ['Firefox on iOS', UA.firefoxIOS],
    ['Edge on iOS', UA.edgeIOS],
  ])('flags %s, which is WebKit under a different brand', (label, ua) => {
    const r = detectBrowserSupport(env(ua));
    expect(r.supported === false && r.reason).toBe('webkit');
    expect(r.supported === false && r.browserLabel).toBe(label);
  });

  // The false-positive guard. macOS/Windows/Android Chrome all carry "Safari"
  // in their UA, and Android reports touch points.
  it.each([
    ['macOS Chrome', UA.macChrome, 0],
    ['macOS Firefox', UA.macFirefox, 0],
    ['macOS Edge', UA.macEdge, 0],
    ['Windows Chrome', UA.winChrome, 0],
    ['Linux Firefox', UA.linuxFirefox, 0],
    ['Android Chrome', UA.androidChrome, 5],
    ['macOS headless Chrome', UA.macHeadlessChrome, 0],
  ])('treats %s as supported', (_label, ua, touch) => {
    expect(detectBrowserSupport(env(ua, { maxTouchPoints: touch })).supported).toBe(true);
  });

  it('flags a browser with no WebTransport at all', () => {
    const r = detectBrowserSupport(env(UA.winChrome, { hasWebTransport: false }));
    expect(r.supported).toBe(false);
    expect(r.supported === false && r.reason).toBe('no-webtransport');
  });

  // WebKit is the specific, known defect and gets the specific message even
  // though older Safari also lacks the API.
  it('reports webkit rather than no-webtransport when both apply', () => {
    const r = detectBrowserSupport(env(UA.macSafari, { hasWebTransport: false }));
    expect(r.supported === false && r.reason).toBe('webkit');
  });

  it('does not flag an unrecognized user agent that has WebTransport', () => {
    expect(detectBrowserSupport(env('some-unknown-agent/1.0')).supported).toBe(true);
  });
});
