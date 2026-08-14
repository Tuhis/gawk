// R37 (docs/40 §4.7): the one view-link builder both screens copy from.
// Broadcast IDs are per-relay, so a link minted while connected to a
// non-default relay must carry that relay (`?relay=`) or it only works for
// viewers who happen to point at the same server. On the deployment default
// the link stays exactly as short as it always was.

import { defaultServerUrl, useTransportStore } from '../state/transportStore';
import { normalizeRelayOrigin } from './relayUrl';

export function buildViewLink(broadcastId: string): string {
  const base = `${window.location.origin}${window.location.pathname}#/view/${broadcastId}`;
  const { serverUrl } = useTransportStore.getState();
  const resolved = normalizeRelayOrigin(serverUrl);
  if (resolved === null || resolved === normalizeRelayOrigin(defaultServerUrl())) return base;
  return `${base}?relay=${encodeURIComponent(resolved)}`;
}
