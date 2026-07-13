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
  return 5;
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
