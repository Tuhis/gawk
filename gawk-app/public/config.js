// Runtime configuration for gawk-app. Served at /config.js and loaded before
// the app bundle. This shipped default is intentionally empty — the gawk-app
// Helm chart overrides this file (via a ConfigMap) from its `config.*` values.
//
// Recognized keys (see src/config.ts):
//   requirePublishSecret: boolean  — prompt broadcasters for the relay's
//                                    publish secret on "Start a stream".
//   maxDecoderQueueSize: number    — viewer decoder queue bound.
//   dvrBufferMs: number            — R21 Deep buffer floor, ms.
//   termsVersion: string           — R23 terms version; bump to re-prompt.
//   operatorName: string           — shown as the operating party in the terms.
//   operatorContact: string        — terms contact point.
//   termsUrl: string               — full terms-body override URL (e.g.
//                                    "/terms.html"); empty ⇒ bundled default.
window.__GAWK_CONFIG__ = window.__GAWK_CONFIG__ || {};
