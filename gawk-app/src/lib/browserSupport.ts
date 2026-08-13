// Which browsers can actually watch a gawk stream (BUGS.md: "Every WebKit
// viewer fails to join since the quic-go bump").
//
// **Why this sniffs the user agent instead of feature-detecting.** The defect
// is not a missing API — WebKit ships `WebTransport`, and every capability
// probe the app already runs passes there. It refuses during HTTP/3 SETTINGS
// negotiation, before the extended CONNECT, so the only honest feature test is
// "open a session and see", which costs a full dial and still lands the user in
// the failure we are trying to warn them about. Until the relay's QUIC stack
// and WebKit agree again, the engine is the signal.
//
// **The unit is the engine, not the brand.** On iOS and iPadOS every browser is
// WebKit underneath — Chrome, Firefox and Edge there are re-skins of the same
// engine and fail identically. On macOS only Safari is. Note that this asks a
// different question from `telemetry.describeClient`, which classifies clients
// coarsely for diagnostics; that one deliberately cannot tell a WebKit re-brand
// from the browser whose name it wears, which is exactly the distinction here.

export interface BrowserEnv {
  userAgent: string;
  // navigator.maxTouchPoints — the only way to tell an iPad from a Mac, since
  // iPadOS 13+ sends a desktop "Macintosh" user agent.
  maxTouchPoints: number;
  hasWebTransport: boolean;
}

export type UnsupportedReason = 'webkit' | 'no-webtransport';

export type BrowserSupport =
  | { supported: true }
  | { supported: false; reason: UnsupportedReason; browserLabel: string };

const IOS_DEVICE = /\b(iPhone|iPad|iPod)\b/;
const MACINTOSH = /\bMacintosh\b/;

// Every Chromium and Gecko browser's user agent also ends in "Safari/537.36",
// so the presence of "Safari" identifies nothing. The *absence* of these brands
// is what identifies real Safari. `Edg/` does not match `EdgiOS/`, and
// `Chrome/` does not match `CriOS/` — the trailing slash is load-bearing.
//
// `HeadlessChrome` is listed separately because `\bChrome\/` does NOT match it
// (no word boundary inside "HeadlessChrome"), which is the same trap
// telemetry.ts's browserClass documents. Without it, headless Chrome on macOS —
// a UA with "Macintosh", "Safari/" and no recognizable Chromium brand — is
// misread as Safari and every such run gets the warning modal.
const NON_WEBKIT_BRAND = /\b(HeadlessChrome|Chrome|Chromium|Firefox|Edg|OPR)\//;

// The iOS re-brands, in the order they appear on the tin.
const IOS_BRANDS: [RegExp, string][] = [
  [/\bCriOS\//, 'Chrome on iOS'],
  [/\bFxiOS\//, 'Firefox on iOS'],
  [/\bEdgiOS\//, 'Edge on iOS'],
  [/\bOPiOS\//, 'Opera on iOS'],
];

// A Mac reporting touch points is an iPad sending the desktop user agent; real
// Macs have no touchscreen. Guarded at >1 rather than >0 so a stray single
// pointer never misclassifies a desktop.
function isIOSFamily(ua: string, maxTouchPoints: number): boolean {
  return IOS_DEVICE.test(ua) || (MACINTOSH.test(ua) && maxTouchPoints > 1);
}

export function isWebKitEngine(ua: string, maxTouchPoints: number): boolean {
  if (isIOSFamily(ua, maxTouchPoints)) return true;
  return MACINTOSH.test(ua) && /\bSafari\//.test(ua) && !NON_WEBKIT_BRAND.test(ua);
}

function webKitLabel(ua: string, maxTouchPoints: number): string {
  if (isIOSFamily(ua, maxTouchPoints)) {
    for (const [re, label] of IOS_BRANDS) {
      if (re.test(ua)) return label;
    }
    return 'Safari on iOS';
  }
  return 'Safari';
}

export function detectBrowserSupport(env: BrowserEnv): BrowserSupport {
  const { userAgent, maxTouchPoints, hasWebTransport } = env;
  // WebKit first: it is the specific, known defect and earns the specific
  // message, even on an older WebKit that also lacks the API outright.
  if (isWebKitEngine(userAgent, maxTouchPoints)) {
    return {
      supported: false,
      reason: 'webkit',
      browserLabel: webKitLabel(userAgent, maxTouchPoints),
    };
  }
  if (!hasWebTransport) {
    return { supported: false, reason: 'no-webtransport', browserLabel: 'This browser' };
  }
  return { supported: true };
}

export function readBrowserEnv(): BrowserEnv {
  const nav = typeof navigator !== 'undefined' ? navigator : undefined;
  return {
    userAgent: nav?.userAgent ?? '',
    maxTouchPoints: nav?.maxTouchPoints ?? 0,
    hasWebTransport: typeof WebTransport !== 'undefined',
  };
}
