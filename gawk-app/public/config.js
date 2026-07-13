// Runtime configuration for gawk-app. Served at /config.js and loaded before
// the app bundle. This shipped default is intentionally empty — the gawk-app
// Helm chart overrides this file (via a ConfigMap) from its `config.*` values.
//
// Recognized keys (see src/config.ts):
//   requirePublishSecret: boolean  — prompt broadcasters for the relay's
//                                    publish secret on "Start a stream".
window.__GAWK_CONFIG__ = window.__GAWK_CONFIG__ || {};
