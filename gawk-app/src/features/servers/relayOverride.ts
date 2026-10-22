// Route → session-override wiring. Called synchronously when the route
// changes, BEFORE the new route's screen renders: the viewer/broadcaster
// connection effects dial `serverUrl` on mount, so the override must already
// be resolved by then (a dial to the default relay followed by a reconnect
// would be a real connection to a server the link did not name).
//
// The override is route-scoped: a route without a (usable) relay parameter
// clears it, so a link's relay drives that session and nothing after it.

import { allowCustomRelays } from '../../config';
import type { Route } from '../../routing';
import { useTransportStore } from '../../state/transportStore';

export const NOTE_RELAY_NOT_ALLOWED =
  'This link asked to use a different server, but this deployment only allows its own.';
export const NOTE_RELAY_INVALID =
  'This link named a server it couldn’t be understood as — joining on your own server instead.';
// R74 (docs/76 OD12): a certificate pin is honoured only beside a relay named
// by a private-range IP address. The session still dials the relay, so a
// relay with a real certificate works; a self-signed one will not connect.
export const NOTE_CERT_HASH_DROPPED =
  'This link carried a certificate pin, which only works with a server on a local network address — ignoring it.';

export function applyRouteRelay(route: Route): void {
  const store = useTransportStore.getState();
  // A new route is a new session: any foreign-telemetry disclosure belongs
  // to the session that produced it.
  store.setForeignTelemetryActive(false);
  // Room links take the same ?relay= parameter, and the join resolver carries
  // it forward to whichever surface it lands on. The landing page takes it
  // too (R74): a standalone PoP's join link is `#/?relay=…&certHash=…`, and
  // the page's own hops carry it on (relayQuerySuffix).
  if (
    route.view !== 'landing' &&
    route.view !== 'viewer' &&
    route.view !== 'broadcaster' &&
    route.view !== 'room' &&
    route.view !== 'join'
  ) {
    store.setSessionOverride(null);
    store.setRelayLinkNote(null);
    return;
  }
  if (route.relay !== null && allowCustomRelays()) {
    store.setSessionOverride(route.relay, route.certHashes);
    store.setRelayLinkNote(route.droppedParams.includes('certHash') ? NOTE_CERT_HASH_DROPPED : null);
    return;
  }
  store.setSessionOverride(null);
  if (route.relay !== null) {
    // Valid value, gated deployment: the link still works, on the
    // deployment's own relay, with a quiet note.
    store.setRelayLinkNote(NOTE_RELAY_NOT_ALLOWED);
  } else if (route.droppedParams.includes('relay')) {
    store.setRelayLinkNote(NOTE_RELAY_INVALID);
  } else {
    store.setRelayLinkNote(null);
  }
}
