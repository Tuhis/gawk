// Share links. Broadcast IDs are per relay, so a link minted on a non-default
// relay carries it (`?relay=`); on the deployment default it stays short.

import { defaultServerUrl, useTransportStore } from '../state/transportStore';
import { normalizeRelayOrigin } from './relayUrl';

// The `?relay=…` suffix naming the resolved relay, or '' on the deployment
// default. In-app hops (#/join → #/room, room → #/broadcast) use it too, so a
// session on a non-default relay stays on it.
export function relayQuerySuffix(): string {
  const { serverUrl } = useTransportStore.getState();
  const resolved = normalizeRelayOrigin(serverUrl);
  if (resolved === null || resolved === normalizeRelayOrigin(defaultServerUrl())) return '';
  return `?relay=${encodeURIComponent(resolved)}`;
}

export function buildViewLink(broadcastId: string): string {
  return `${window.location.origin}${window.location.pathname}#/view/${broadcastId}${relayQuerySuffix()}`;
}

// Never carries a grant: the `?rt=` hand-off is for a native broadcaster's
// own launch only.
export function buildRoomLink(code: string): string {
  return `${window.location.origin}${window.location.pathname}#/room/${encodeURIComponent(code)}${relayQuerySuffix()}`;
}
