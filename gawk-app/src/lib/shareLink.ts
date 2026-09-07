// R37 (docs/40 §4.7): the one view-link builder both screens copy from.
// Broadcast IDs are per-relay, so a link minted while connected to a
// non-default relay must carry that relay (`?relay=`) or it only works for
// viewers who happen to point at the same server. On the deployment default
// the link stays exactly as short as it always was.

import { defaultServerUrl, useTransportStore } from '../state/transportStore';
import { normalizeRelayOrigin } from './relayUrl';

// The `?relay=…` suffix a link needs to name the resolved relay, or '' on the
// deployment default. Shared by every link builder and by the R42 in-app
// hops (#/join → #/room, room → #/broadcast) so a session on a non-default
// relay stays on it across the hop.
export function relayQuerySuffix(): string {
  const { serverUrl } = useTransportStore.getState();
  const resolved = normalizeRelayOrigin(serverUrl);
  if (resolved === null || resolved === normalizeRelayOrigin(defaultServerUrl())) return '';
  return `?relay=${encodeURIComponent(resolved)}`;
}

export function buildViewLink(broadcastId: string): string {
  return `${window.location.origin}${window.location.pathname}#/view/${broadcastId}${relayQuerySuffix()}`;
}

// R42 (docs/44 §4.9): the room link, same relay rule. Never carries a grant —
// the `?rt=` hand-off is for a native broadcaster's own launch only.
export function buildRoomLink(code: string): string {
  return `${window.location.origin}${window.location.pathname}#/room/${encodeURIComponent(code)}${relayQuerySuffix()}`;
}
