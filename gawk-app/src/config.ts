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
