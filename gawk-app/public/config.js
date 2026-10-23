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
//   relayCertHashes: string[]      — R74: hex SHA-256 hashes of relayUrl's
//                                    pinned self-signed certificate DER
//                                    (current + next across a rotation). A
//                                    PoP's generated /app/config.js sets it
//                                    beside a LAN-address relayUrl, with
//                                    requirePublishSecret: false. Applies to
//                                    relayUrl only; not dev-gated.
//
// DEV-ONLY (R38, docs/41 D4) — read only when the page is served from
// localhost, and deliberately NOT a chart value; the local docker compose
// stack renders it via dev/config-gen.sh:
//   devCertHashHex: string         — hex SHA-256 of the relay's self-signed
//                                    dev certificate DER. A deployment that
//                                    thinks it needs this has a TLS
//                                    misconfiguration, not a missing knob.
window.__GAWK_CONFIG__ = window.__GAWK_CONFIG__ || {};
