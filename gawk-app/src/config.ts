// Runtime configuration for the static SPA. The gawk-app Helm chart renders a
// `/config.js` (a ConfigMap mounted over the shipped default) that sets
// `window.__GAWK_CONFIG__` before the app bundle runs — so deploy-time flags
// reach the frontend without rebuilding the image or adding a server endpoint.
// The shipped default (public/config.js) is empty, so dev + un-configured
// installs fall back to the defaults below.

export interface GawkRuntimeConfig {
  // The relay requires a pre-shared publish secret (server started with
  // -publish-secret). When true, the broadcaster asks for it on "Start a
  // stream". Default false.
  requirePublishSecret?: boolean;
  
  // The maximum number of frames the viewer's WebCodecs VideoDecoder will queue
  // before it starts dropping frames until the next keyframe.
  maxDecoderQueueSize?: number;

  // R21 (docs/26): the playout floor a Deep buffer viewer holds, in ms, and
  // the value it asks the relay to back. Pairs with the relay chart's
  // config.dvrWindow, which clamps it — so this is the knob for tuning the
  // trade per deployment without an image rebuild, which is exactly what DV6
  // needs. Default 3000.
  dvrBufferMs?: number;

  // R23 (docs/29): terms & conditions. Bumping the version re-prompts every
  // broadcaster for acknowledgment (D7); empty/unset falls back to the
  // BUNDLED_TERMS_VERSION baked into this release. A date-stamp is the
  // recommended form, e.g. "2026-07-24".
  termsVersion?: string;

  // Substituted into the bundled default terms text so an operator gets
  // correct attribution + a contact point without editing prose. Empty/unset
  // shows neutral placeholders. Ignored when a full body override is supplied.
  operatorName?: string;
  operatorContact?: string;

  // Optional full-body terms override. When set, the terms page renders this
  // document (fetched on route open, sanitized) instead of the bundled
  // default. Recommended shape: "/terms.html", a ConfigMap-mounted static
  // asset (see the gawk-app chart). An absolute URL is allowed.
  termsUrl?: string;
}

declare global {
  interface Window {
    __GAWK_CONFIG__?: GawkRuntimeConfig;
  }
}

export function getRuntimeConfig(): GawkRuntimeConfig {
  return (typeof window !== 'undefined' && window.__GAWK_CONFIG__) || {};
}

export function requiresPublishSecret(): boolean {
  const config = getRuntimeConfig();
  if (config.requirePublishSecret !== undefined) {
    return config.requirePublishSecret;
  }
  return isDevEnvironment();
}

export function getMaxDecoderQueueSize(): number {
  const config = getRuntimeConfig();
  if (config.maxDecoderQueueSize !== undefined) {
    return config.maxDecoderQueueSize;
  }
  // 10 (raised from 5, R10): a decoder that is briefly ~one burst behind now
  // absorbs it instead of cycling overflow → drop-to-keyframe → GOP wait.
  // Worst case this queues ~10 frames ≈ 330 ms at 30 fps before the resync
  // policy kicks in — acceptable against a 500 ms GOP recovery.
  return 10;
}

// "Debug build" per the product spec = running locally. Vite dev mode, or a
// production bundle served from a loopback host. Gates the developer-only
// settings (server URL, dev cert hash) so real users never see them.
export function isDevEnvironment(): boolean {
  if (import.meta.env.DEV) return true;
  if (typeof window === 'undefined') return false;
  const h = window.location.hostname;
  return h === 'localhost' || h === '127.0.0.1' || h === '::1' || h === '[::1]';
}

export const DEFAULT_DVR_BUFFER_MS = 3000;
// The relay downgrades anything under a second (docs/26 Decision 7), so asking
// for less is asking for plain carrier delivery by another name.
export const MIN_DVR_BUFFER_MS = 1000;
export const MAX_DVR_BUFFER_MS = 30000;

// R21: the Deep buffer floor. Clamped to something a viewer can actually hold
// and a relay can plausibly back — a value below the relay's minimum would be
// silently downgraded, and one in the minutes is a configuration error rather
// than a choice.
export function getDvrBufferMs(): number {
  const v = getRuntimeConfig().dvrBufferMs;
  if (typeof v !== 'number' || !Number.isFinite(v)) return DEFAULT_DVR_BUFFER_MS;
  return Math.min(Math.max(Math.round(v), MIN_DVR_BUFFER_MS), MAX_DVR_BUFFER_MS);
}

// R23 (docs/29): terms & conditions. The version baked into this release; the
// acknowledgment key stores whichever version a broadcaster last agreed to, so
// an operator bumps config.termsVersion to re-prompt on a meaningful edit (D7).
export const BUNDLED_TERMS_VERSION = '2026-07-24';

// Empty string counts as unset — the ConfigMap renders an empty default rather
// than duplicating the version constant, so an un-configured install falls
// back to the bundled version here.
export function getTermsVersion(): string {
  const v = getRuntimeConfig().termsVersion;
  return (typeof v === 'string' && v.trim()) || BUNDLED_TERMS_VERSION;
}

// Neutral fallback when an operator hasn't set a name — the bundled text still
// reads correctly ("operated by the operator of this deployment").
export function getOperatorName(): string {
  const v = getRuntimeConfig().operatorName;
  return (typeof v === 'string' && v.trim()) || 'the operator of this deployment';
}

// Null when unconfigured, so the contact clause can degrade gracefully rather
// than print an empty address.
export function getOperatorContact(): string | null {
  const v = getRuntimeConfig().operatorContact;
  return (typeof v === 'string' && v.trim()) || null;
}

// Null when unconfigured ⇒ the terms page renders the bundled default and
// never issues a network request (keeps the app's boot path fetch-free).
export function getTermsUrl(): string | null {
  const v = getRuntimeConfig().termsUrl;
  return (typeof v === 'string' && v.trim()) || null;
}
