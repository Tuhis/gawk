// Which browsers can actually watch a gawk stream.
//
// Feature detection only: `WebTransport` is the one API every stream rides on,
// and a browser without it cannot join, so the app says so before the user
// hits an opaque "streamer offline" card. No user-agent sniffing: that is the
// tool for a known engine defect, and there is none today.

export interface BrowserEnv {
  hasWebTransport: boolean;
}

export type UnsupportedReason = 'no-webtransport';

export type BrowserSupport =
  | { supported: true }
  | { supported: false; reason: UnsupportedReason; browserLabel: string };

export function detectBrowserSupport(env: BrowserEnv): BrowserSupport {
  if (!env.hasWebTransport) {
    return { supported: false, reason: 'no-webtransport', browserLabel: 'This browser' };
  }
  return { supported: true };
}

export function readBrowserEnv(): BrowserEnv {
  return { hasWebTransport: typeof WebTransport !== 'undefined' };
}
