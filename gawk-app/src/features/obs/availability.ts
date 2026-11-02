// R33 (docs/83 U3, OD4): where streaming from OBS can work. The gateway
// publishes only to this deployment's own relay, so it needs both a
// configured `whipGatewayUrl` and the default server selected (by
// normalized URL, like resolvedUrlIsDefault: a saved duplicate of the
// deployment's relay is still the deployment's relay). The way in on the
// broadcaster page also needs a desktop, since OBS doesn't run on the
// devices where R67 hides its offer.
import { getWhipGatewayUrl } from '../../config';
import { isDesktopForHandoff } from '../../lib/desktopLink';
import { normalizeRelayOrigin } from '../../lib/relayUrl';
import { defaultServerUrl } from '../../state/transportStore';

export function obsIngestAvailable(serverUrl: string): boolean {
  if (getWhipGatewayUrl() === '') return false;
  const resolved = normalizeRelayOrigin(serverUrl);
  return resolved !== null && resolved === normalizeRelayOrigin(defaultServerUrl());
}

export function obsWayInOffered(serverUrl: string): boolean {
  if (!obsIngestAvailable(serverUrl)) return false;
  if (typeof navigator === 'undefined') return false;
  return isDesktopForHandoff(navigator.userAgent, navigator.maxTouchPoints ?? 0);
}
