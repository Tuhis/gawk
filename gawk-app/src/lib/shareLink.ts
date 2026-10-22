// Share links. Broadcast IDs are per relay, so a link minted on a non-default
// relay carries it (`?relay=`); on the deployment default it stays short.

import { MAX_CERT_HASHES, isPinnableRelay, parseCertHashList } from '../routing';
import { defaultServerUrl, useTransportStore } from '../state/transportStore';
import { normalizeRelayOrigin } from './relayUrl';

// The `?relay=…` suffix naming the resolved relay, or '' on the deployment
// default. In-app hops (#/join → #/room, room → #/broadcast) use it too, so a
// session on a non-default relay stays on it. Beside a private-literal relay
// it also carries the resolved pins as `&certHash=` (R74, docs/76 §6.1 item
// 5): that is a standalone PoP, whose self-signed certificate a guest's
// browser can only dial with them. Anywhere else the router would drop the
// pins, so they are never written.
export function relayQuerySuffix(): string {
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

export function buildViewLink(broadcastId: string): string {
  return `${window.location.origin}${window.location.pathname}#/view/${broadcastId}${relayQuerySuffix()}`;
}

// Never carries a grant: the `?rt=` hand-off is for a native broadcaster's
// own launch only.
export function buildRoomLink(code: string): string {
  return `${window.location.origin}${window.location.pathname}#/room/${encodeURIComponent(code)}${relayQuerySuffix()}`;
}
