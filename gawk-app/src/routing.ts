// Pure hash-route parsing for the production UI (docs/10 Decision 1). Three
// production surfaces plus the frozen debug tree. Kept pure and unit-tested;
// App.tsx subscribes to hashchange and renders the result.
//
// R37 (docs/40 §4.2): the production routes accept a query. The query is
// split off before path matching (the seam R26 QL1 also specifies — the two
// grammars are disjoint and both inherit R26 D7: invalid or unknown
// parameters are ignored, never fatal, and collected into a quiet note).
// `relay` is the one parameter this milestone defines: an https origin that
// becomes a session-only server override.
import { isValidBroadcastId } from './lib/broadcastId';
import { normalizeRelayOrigin } from './lib/relayUrl';
import { isValidRoomCode } from './lib/roomCode';

export interface RouteQuery {
  // Normalized https origin from a valid ?relay= value, else null.
  relay: string | null;
  // Quiet-note fodder (R26 D7): parameters that were present but unusable —
  // today that's only an invalid relay value. Unknown parameters are ignored
  // silently here and left for the R26 grammar to claim.
  droppedParams: string[];
}

export type Route =
  | { view: 'landing' }
  | ({ view: 'broadcaster' } & RouteQuery)
  | ({ view: 'viewer'; broadcastId: string } & RouteQuery)
  // R42 (docs/44 D19): a room link. The code is kept exactly as typed — a
  // static slug displays as configured and the relay normalizes it — and
  // validated as a slug (3–32 of [A-Za-z0-9-]) or a broadcast-shaped code.
  // `grant` is the one-shot `?rt=` hand-off (docs/44 §4.8): App.tsx moves it
  // into session storage and rewrites the hash before the first render, so
  // it never sits in a URL long enough to be copied.
  | ({ view: 'room'; code: string; grant: string | null } & RouteQuery)
  // R42 (docs/44 D19): a typed six-character code resolving to whichever of
  // room / broadcast it names. The relay decides; the SPA tries `/room/`
  // first, then falls back to `#/view/`. Broadcast-alphabet codes only: a
  // static slug typed into the join box is refused client-side, which is
  // the "link only" decision made visible (docs/44 §4.2).
  | ({ view: 'join'; code: string } & RouteQuery)
  | { view: 'terms' }
  | { view: 'debug-index' }
  | { view: 'debug-broadcast' }
  | { view: 'debug-view' }
  | { view: 'debug-loopback' }
  // #/view with no/invalid id, or anything unknown, sends the user to the
  // landing page (which owns code entry).
  | { view: 'redirect'; to: string };

export const HOME = '#/';

function parseQuery(query: string): RouteQuery {
  const out: RouteQuery = { relay: null, droppedParams: [] };
  if (query === '') return out;
  let params: URLSearchParams;
  try {
    params = new URLSearchParams(query);
  } catch {
    return out;
  }
  const relay = params.get('relay');
  if (relay !== null) {
    const normalized = normalizeRelayOrigin(relay);
    if (normalized !== null) {
      out.relay = normalized;
    } else {
      // A typo'd relay in a long-lived link degrades to "joins on the user's
      // own server", surfaced quietly — never to "cannot join" (D7 of R26,
      // restated as docs/40 §4.2).
      out.droppedParams.push('relay');
    }
  }
  return out;
}

// The `?rt=` grant on a room link, verbatim (its shape is the hand-off
// module's business — features/room/grantHandoff.ts). Empty ⇒ absent.
function parseGrant(query: string): string | null {
  if (query === '') return null;
  try {
    const v = new URLSearchParams(query).get('rt');
    return v !== null && v !== '' ? v : null;
  } catch {
    return null;
  }
}

// Strip the one-shot `?rt=` parameter from a hash, leaving everything else
// (the path, `?relay=`) byte-identical. Pure, so the hand-off's rewrite is
// unit-testable without a window.
export function hashWithoutGrant(hash: string): string {
  const qIndex = hash.indexOf('?');
  if (qIndex === -1) return hash;
  const params = new URLSearchParams(hash.slice(qIndex + 1));
  params.delete('rt');
  const rest = params.toString();
  return rest === '' ? hash.slice(0, qIndex) : `${hash.slice(0, qIndex)}?${rest}`;
}

export function parseRoute(hash: string): Route {
  // Split any query off before path matching — `#/view/AB2CD3?relay=…` must
  // match exactly like `#/view/AB2CD3`.
  const raw = hash.replace(/^#/, '');
  const qIndex = raw.indexOf('?');
  const query = qIndex === -1 ? '' : raw.slice(qIndex + 1);
  // Normalize: strip a leading '/', collapse trailing '/'.
  const path = (qIndex === -1 ? raw : raw.slice(0, qIndex)).replace(/^\//, '').replace(/\/+$/, '');

  if (path === '') return { view: 'landing' };
  if (path === 'broadcast') return { view: 'broadcaster', ...parseQuery(query) };
  // R23 (docs/29): the terms surface, reachable from every surface, gated
  // behind nothing.
  if (path === 'terms') return { view: 'terms' };

  if (path === 'view' || path.startsWith('view/')) {
    const id = path.slice('view/'.length).toUpperCase();
    if (path !== 'view' && isValidBroadcastId(id)) {
      return { view: 'viewer', broadcastId: id, ...parseQuery(query) };
    }
    return { view: 'redirect', to: HOME };
  }

  if (path === 'room' || path.startsWith('room/')) {
    const code = path.slice('room/'.length);
    if (path !== 'room' && isValidRoomCode(code)) {
      return { view: 'room', code, grant: parseGrant(query), ...parseQuery(query) };
    }
    return { view: 'redirect', to: HOME };
  }

  if (path === 'join' || path.startsWith('join/')) {
    const code = path.slice('join/'.length).toUpperCase();
    if (path !== 'join' && isValidBroadcastId(code)) {
      return { view: 'join', code, ...parseQuery(query) };
    }
    return { view: 'redirect', to: HOME };
  }

  if (path === 'debug') return { view: 'debug-index' };
  if (path === 'debug/broadcast') return { view: 'debug-broadcast' };
  // The debug viewer keeps its own #/debug/view/<id> namespace so its internal
  // hash sync never collides with the production viewer's #/view/<id>.
  if (path === 'debug/view' || path.startsWith('debug/view/')) return { view: 'debug-view' };
  if (path === 'debug/loopback') return { view: 'debug-loopback' };

  return { view: 'redirect', to: HOME };
}
