// Runtime configuration for the static SPA. The gawk-app Helm chart renders a
// `/config.js` (a ConfigMap mounted over the shipped default) that sets
// `window.__GAWK_CONFIG__` before the app bundle runs — so deploy-time flags
// reach the frontend without rebuilding the image or adding a server endpoint.
// The shipped default (public/config.js) is empty, so dev + un-configured
// installs fall back to the defaults below.

export interface GawkRuntimeConfig {
  // Where this deployment's relay lives, e.g. "https://relay.example.com:4433".
  // Unset falls back to transportStore's origin-derived default, which keeps
  // local dev (localhost:5173 → localhost:4433) working with no config.
  relayUrl?: string;

  // The relay requires a pre-shared publish secret (server started with
  // -publish-secret). When true, the broadcaster asks for it on "Start a
  // stream". Default false.
  requirePublishSecret?: boolean;
  
  // The maximum number of frames the viewer's WebCodecs VideoDecoder will queue
  // before it starts dropping frames until the next keyframe.
  maxDecoderQueueSize?: number;

  // The playout floor a Deep buffer viewer holds, in ms, and the value it asks
  // the relay to back (the relay chart's config.dvrWindow clamps it).
  // Default 3000.
  dvrBufferMs?: number;

  // Terms version. Bumping it re-prompts every broadcaster; empty/unset falls
  // back to BUNDLED_TERMS_VERSION. A date stamp is the recommended form.
  termsVersion?: string;

  // Substituted into the bundled default terms text so an operator gets
  // correct attribution + a contact point without editing prose. Empty/unset
  // shows neutral placeholders. Ignored when a full body override is supplied.
  operatorName?: string;
  operatorContact?: string;

  // Where telemetry batches are POSTed. The default is a same-origin path,
  // which lets the unload `sendBeacon` flush work without a CORS preflight;
  // another origin must answer preflights itself. Whether anything is sent at
  // all is decided by the relay (the telemetry hello), not by this value.
  telemetryUrl?: string;

  // Optional full-body terms override. When set, the terms page renders this
  // document (fetched on route open, sanitized) instead of the bundled
  // default. Recommended shape: "/terms.html", a ConfigMap-mounted static
  // asset (see the gawk-app chart). An absolute URL is allowed.
  termsUrl?: string;

  // Whether this UI may talk to relays other than its own (the server picker
  // and ?relay= links). Default true; false hides the picker and ignores
  // ?relay= with a quiet note.
  allowCustomRelays?: boolean;

  // Hex SHA-256 of a local stack's self-signed relay certificate (rendered by
  // dev/config-gen.sh), so #/view/{id} works in a fresh profile. Read only in a
  // dev environment and deliberately not a chart value: a production
  // deployment that needs it has a TLS problem, not a missing knob.
  devCertHashHex?: string;

  // Optional server directory JSON the picker offers, fetched when the picker
  // opens, never at boot. Same-origin recommended; a cross-origin URL must
  // serve CORS itself.
  serverDirectoryUrl?: string;
}

declare global {
  interface Window {
    __GAWK_CONFIG__?: GawkRuntimeConfig;
  }
}

export function getRuntimeConfig(): GawkRuntimeConfig {
  return (typeof window !== 'undefined' && window.__GAWK_CONFIG__) || {};
}

// The relay this deployment talks to, or '' when nothing is configured — in
// which case transportStore falls back to its origin-derived default. Kept
// here rather than in transportStore so every runtime knob has one home, and
// so the "empty string counts as unset" rule is the same as every other
// getter's (the ConfigMap renders "" rather than omitting the key).
export function getRelayUrl(): string {
  const v = getRuntimeConfig().relayUrl;
  return (typeof v === 'string' && v.trim()) || '';
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
  // Enough to absorb a decoder briefly one burst behind instead of cycling
  // overflow → drop to keyframe → GOP wait; at worst ~330 ms at 30 fps, less
  // than a 500 ms GOP recovery.
  return 10;
}

// A missing key allows other relays, so shared ?relay= links work on installs
// whose operator configured nothing.
export function allowCustomRelays(): boolean {
  const v = getRuntimeConfig().allowCustomRelays;
  return v === undefined ? true : v;
}

// '' when unset: no directory section in the picker.
export function getServerDirectoryUrl(): string {
  const v = getRuntimeConfig().serverDirectoryUrl;
  return (typeof v === 'string' && v.trim()) || '';
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

// The local stack's relay certificate hash, or '' outside a dev environment:
// a bundle served from a real hostname must never present a configured hash.
export function getDevCertHashHex(): string {
  if (!isDevEnvironment()) return '';
  const v = getRuntimeConfig().devCertHashHex;
  return (typeof v === 'string' && v.trim()) || '';
}

export const DEFAULT_DVR_BUFFER_MS = 3000;
// The relay downgrades anything under a second, so asking for less is plain
// carrier delivery by another name.
export const MIN_DVR_BUFFER_MS = 1000;
export const MAX_DVR_BUFFER_MS = 30000;

// The Deep buffer floor, clamped to what a viewer can hold and a relay can
// plausibly back.
export function getDvrBufferMs(): number {
  const v = getRuntimeConfig().dvrBufferMs;
  if (typeof v !== 'number' || !Number.isFinite(v)) return DEFAULT_DVR_BUFFER_MS;
  return Math.min(Math.max(Math.round(v), MIN_DVR_BUFFER_MS), MAX_DVR_BUFFER_MS);
}

// Relative, so no CORS, second DNS name or certificate is involved.
export const DEFAULT_TELEMETRY_URL = '/api/telemetry/v1/ingest';

export function getTelemetryUrl(): string {
  const v = getRuntimeConfig().telemetryUrl;
  return (typeof v === 'string' && v.trim()) || DEFAULT_TELEMETRY_URL;
}

// Where this software comes from. Deliberately a constant and NOT a runtime
// knob: it points at the project every deployment is built from, not at the
// deployment — a fork that wants its own link edits it in the fork.
export const SOURCE_URL = 'https://github.com/Tuhis/gawk';

// The project site and its downloads (the native broadcasters). Constants for
// the same reason as SOURCE_URL.
export const SITE_URL = 'https://tuhis.github.io/gawk/';
export const SITE_DOWNLOAD_URL = `${SITE_URL}#download`;

// The terms version baked into this release; config.termsVersion overrides it
// to re-prompt broadcasters after a meaningful edit.
export const BUNDLED_TERMS_VERSION = '2026-07-26';

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
