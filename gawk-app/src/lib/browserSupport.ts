// Which browsers can actually watch a gawk stream.
//
// Feature detection only: `WebTransport` is the one API every stream rides on,
// and a browser without it cannot join, so the app says so before the user
// hits an opaque "streamer offline" card. Nothing here sniffs the user agent.
// It used to — between 2026-08-04 and 2026-09-21 WebKit (Safari everywhere,
// and every iOS browser) was warned about by engine, because the relay's
// quic-go / webtransport-go pair refused it during HTTP/3 SETTINGS
// negotiation, upstream of anything a capability probe could see. That was
// fixed in the relay (quic-go v0.62.0 + `webtransport.Server.Config`, see
// docs/gotchas.md), verified against real Safari, and the engine check went
// with it. If a browser-specific refusal ever comes back, the diagnosis has to
// be that specific again — a UA check is the right tool for a known engine
// defect and the wrong tool for anything else.

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
