// Share links. Broadcast IDs are per relay, so a link minted on a non-default
// relay carries it (`?relay=`); on the deployment default it stays short.

import { MAX_CERT_HASHES, isPinnableRelay, parseCertHashList } from '../routing';
import { defaultServerUrl, useTransportStore } from '../state/transportStore';
import { normalizeRelayOrigin } from './relayUrl';
import { usePopStore } from '../features/pop/popStore';

// The public gawk UI a standalone PoP's links point at from a page no guest
// could open (the host's own /app/ on loopback): the official deployment,
// as gawk-relay's console uses (relayconsole.DefaultAppOrigin).
export const REFERENCE_APP_ORIGIN = 'https://gawk.ioio.fi';

// The `?relay=…` suffix naming the resolved relay, or '' on the deployment
// default. In-app hops (#/join → #/room, room → #/broadcast) use it too, so a
// session on a non-default relay stays on it. Beside a private-literal relay
// it also carries the resolved pins as `&certHash=` (R74, docs/76 §6.1 item
// 5): that is a standalone PoP, whose self-signed certificate a guest's
// browser can only dial with them. Anywhere else the router would drop the
// pins, so they are never written.
export function relayQuerySuffix(): string {
  // R74: a tab a followed offer moved to a PoP stays there in memory across
  // hops; naming the PoP in the URL would only pin it into the route.
  if (useTransportStore.getState().resolvedSource === 'pop') return '';
  const relay = nonDefaultRelayOrigin();
  if (relay === null) return '';
  const pins = linkCertHashes(relay);
  return `?relay=${encodeURIComponent(relay)}${pins === '' ? '' : `&certHash=${pins}`}`;
}

// The resolved hashes in the link grammar (routing.ts parseCertHashList), or
// '' when there are none or they would not parse back.
function linkCertHashes(relay: string): string {
  if (!isPinnableRelay(relay)) return '';
  const value = useTransportStore.getState().certHashes.slice(0, MAX_CERT_HASHES).join(',');
  return value !== '' && parseCertHashList(value) !== null ? value : '';
}

// The normalized origin of the resolved relay, or null on the deployment
// default. The desktop handoff link carries it under the same rule
// (docs/69 D3).
export function nonDefaultRelayOrigin(): string | null {
  const { serverUrl } = useTransportStore.getState();
  const resolved = normalizeRelayOrigin(serverUrl);
  if (resolved === null || resolved === normalizeRelayOrigin(defaultServerUrl())) return null;
  return resolved;
}

// The LocalRelayInfo (0x18) of the relay in use, if it is a PoP.
function currentLocalRelayInfo() {
  const rec = usePopStore.getState().info;
  if (rec === null) return null;
  const { serverUrl } = useTransportStore.getState();
  return normalizeRelayOrigin(rec.relayUrl) === normalizeRelayOrigin(serverUrl) ? rec.info : null;
}

// An app origin a link may point at: https, or http on loopback (dev).
function appOrigin(raw: string): string | null {
  try {
    const u = new URL(raw);
    if (u.protocol === 'https:' || (u.protocol === 'http:' && isLoopbackHost(u.hostname))) return u.origin;
  } catch {
    // not a URL
  }
  return null;
}

function isLoopbackHost(host: string): boolean {
  return host === 'localhost' || host === '127.0.0.1' || host === '[::1]';
}

// Where a share link points and what it carries (R74, docs/76 item 45):
// - a linked PoP (its 0x18 names the home app): a home link, no ?relay=;
// - a tab a followed offer moved to the PoP: a home link on this page;
// - a standalone PoP seen from the host's loopback /app/: the public app,
//   naming the LAN relay with its pins;
// - anything else: this page, with ?relay= off the default (relayQuerySuffix).
function shareTarget(): { base: string; suffix: string } {
  const here = `${window.location.origin}${window.location.pathname}`;
  const info = currentLocalRelayInfo();
  if (info?.linked) {
    const home = appOrigin(info.homeOrigin);
    if (home !== null) return { base: home === window.location.origin ? here : `${home}/`, suffix: '' };
  }
  if (useTransportStore.getState().resolvedSource === 'pop') return { base: here, suffix: '' };
  if (info !== null && !info.linked && isLoopbackHost(window.location.hostname)) {
    const relay = normalizeRelayOrigin(useTransportStore.getState().serverUrl);
    if (relay !== null && isPinnableRelay(relay)) {
      const pins = linkCertHashes(relay);
      const app = appOrigin(info.homeOrigin) ?? REFERENCE_APP_ORIGIN;
      return {
        base: `${app}/`,
        suffix: `?relay=${encodeURIComponent(relay)}${pins === '' ? '' : `&certHash=${pins}`}`,
      };
    }
  }
  return { base: here, suffix: relayQuerySuffix() };
}

export function buildViewLink(broadcastId: string): string {
  const { base, suffix } = shareTarget();
  return `${base}#/view/${broadcastId}${suffix}`;
}

// Never carries a grant: the `?rt=` hand-off is for a native broadcaster's
// own launch only.
export function buildRoomLink(code: string): string {
  const { base, suffix } = shareTarget();
  return `${base}#/room/${encodeURIComponent(code)}${suffix}`;
}
