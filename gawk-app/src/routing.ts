// Pure hash-route parsing; App.tsx subscribes to hashchange and renders the
// result. Production routes accept a query, split off before path matching.
// Invalid or unknown parameters are ignored, never fatal: an unusable one is
// only reported as a quiet note.
import { isValidBroadcastId } from './lib/broadcastId';
import { normalizeRelayOrigin } from './lib/relayUrl';
import { isValidRoomCode } from './lib/roomCode';

export interface RouteQuery {
  // Normalized https origin from a valid ?relay= value, else null.
  relay: string | null;
  // Parameters that were present but unusable (today only an invalid relay),
  // for the quiet note. Unknown parameters are ignored silently.
  droppedParams: string[];
}

export type Route =
  | { view: 'landing' }
  | ({ view: 'broadcaster' } & RouteQuery)
  | ({ view: 'viewer'; broadcastId: string } & RouteQuery)
  // A room link. The code is kept as typed (a static slug displays as
  // configured; the relay normalizes it). `grant` is the one-shot `?rt=`
  // hand-off, moved out of the URL before the first render.
  | ({ view: 'room'; code: string; grant: string | null } & RouteQuery)
  // A typed six-character code naming a room or a broadcast; the relay
  // decides. Broadcast-alphabet codes only: static room slugs are link-only.
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
      // own server", surfaced quietly, never to "cannot join".
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

// Strip the one-shot `?rt=` parameter from a hash, keeping the path and the
// other parameters.
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
