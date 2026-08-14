// R37 (docs/40 §4.2): route → session-override wiring. Called synchronously
// when the route changes, BEFORE the new route's screen renders — the
// viewer/broadcaster connection effects dial `serverUrl` on mount, so the
// override must already be resolved by then (a dial to the default relay
// followed by a reconnect would be a real connection the design promises
// never happens).
//
// The override is route-scoped: a route without a (usable) relay parameter
// clears it, which is what makes a link's relay "drive this session" and
// nothing after it (D2).

import { allowCustomRelays } from '../../config';
import type { Route } from '../../routing';
import { useTransportStore } from '../../state/transportStore';

export const NOTE_RELAY_NOT_ALLOWED =
  'This link asked to use a different server, but this deployment only allows its own.';
export const NOTE_RELAY_INVALID =
  'This link named a server it couldn’t be understood as — joining on your own server instead.';

export function applyRouteRelay(route: Route): void {
  const store = useTransportStore.getState();
  // A new route is a new session: any foreign-telemetry disclosure belongs
  // to the session that produced it (D16).
  store.setForeignTelemetryActive(false);
  if (route.view !== 'viewer' && route.view !== 'broadcaster') {
    store.setSessionOverride(null);
    store.setRelayLinkNote(null);
    return;
  }
  if (route.relay !== null && allowCustomRelays()) {
    store.setSessionOverride(route.relay);
    store.setRelayLinkNote(null);
    return;
  }
  store.setSessionOverride(null);
  if (route.relay !== null) {
    // Valid value, gated deployment (D6): the link still works, on the
    // deployment's own relay, with a quiet note.
    store.setRelayLinkNote(NOTE_RELAY_NOT_ALLOWED);
  } else if (route.droppedParams.includes('relay')) {
    store.setRelayLinkNote(NOTE_RELAY_INVALID);
  } else {
    store.setRelayLinkNote(null);
  }
}
