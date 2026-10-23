// Pure hash-route parsing; App.tsx subscribes to hashchange and renders the
// result. Production routes accept a query, split off before path matching.
// Invalid or unknown parameters are ignored, never fatal: an unusable one is
// only reported as a quiet note.
import { isValidBroadcastId } from './lib/broadcastId';
import { normalizeRelayOrigin } from './lib/relayUrl';
import { isValidRoomCode } from './lib/roomCode';
import { sanitizeNickname } from './features/room/roomPrefs';

export interface RouteQuery {
  // Normalized https origin from a valid ?relay= value, else null.
  relay: string | null;
  // R74 (docs/76 §6.1 item 5, OD12): the `?certHash=` pins, 1–2 lowercase
  // hex SHA-256 values of the relay's certificate DER. Non-empty only beside
  // a `relay` whose host is a private-range IP literal (isPinnableRelay).
  certHashes: string[];
  // Parameters that were present but unusable (an invalid relay, a certHash
  // that is malformed or not beside a private literal), for the quiet note.
  // Unknown parameters are ignored silently.
  droppedParams: string[];
}

export type Route =
  // The landing page takes the query too: a standalone PoP's join link is
  // `#/?relay=https://<lan-ip>:4433&certHash=…` (docs/76 §6.1 item 5).
  | ({ view: 'landing' } & RouteQuery)
  // `room` is a room to join once the stream is live (`?room=<code>`, a link
  // from outside the app, such as a chat bot's room card). App.tsx moves it
  // into the room-return stash before the first render. Null when absent or
  // malformed. `nick` is a `?nick=` prefill, as on a room link. `desktop` is
  // `?desktop=1`: the link asks to open the desktop app (R67, docs/69 D9);
  // App.tsx strips it before the first render.
  | ({ view: 'broadcaster'; room: string | null; nick: string | null; desktop: boolean } & RouteQuery)
  | ({ view: 'viewer'; broadcastId: string } & RouteQuery)
  // A room link. The code is kept as typed (a static slug displays as
  // configured; the relay normalizes it). `grant` is the one-shot `?rt=`
  // hand-off, moved out of the URL before the first render. `nick` is a
  // nickname to prefill the prompt with (`?nick=`, e.g. a chat bot's link
  // carrying the user's chat name), sanitized; null when absent or blank.
  | ({ view: 'room'; code: string; grant: string | null; nick: string | null } & RouteQuery)
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

const CERT_HASH_RE = /^[0-9a-f]{64}$/;
export const MAX_CERT_HASHES = 2;

// A `certHash=` value: 1–2 comma-separated lowercase 64-hex hashes, a
// repeated one collapsed. Anything else is null: the whole list is dropped,
// never partially used.
export function parseCertHashList(value: string): string[] | null {
  const parts = value.split(',');
  if (parts.length > MAX_CERT_HASHES) return null;
  const out: string[] = [];
  for (const p of parts) {
    if (!CERT_HASH_RE.test(p)) return null;
    if (!out.includes(p)) out.push(p);
  }
  return out;
}

// True when `hostname` (as `URL.hostname` yields it: canonical dotted IPv4,
// bracketed IPv6) is an IP literal in 10/8, 172.16/12, 192.168/16 or
// fc00::/7. Hostnames never qualify, whatever they look like.
export function isPrivateIpLiteral(hostname: string): boolean {
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(hostname);
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])];
    if ([v4[1], v4[2], v4[3], v4[4]].some((o) => Number(o) > 255)) return false;
    return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168);
  }
  const v6 = /^\[([0-9a-f:.]+)\]$/i.exec(hostname);
  if (v6) {
    // URL serializes IPv6 compressed and lowercase; the first hextet decides
    // fc00::/7. An address starting "::" has a zero first hextet.
    const first = v6[1].toLowerCase().split(':')[0];
    if (first === '' || first.includes('.')) return false;
    const n = parseInt(first, 16);
    return (n & 0xfe00) === 0xfc00;
  }
  return false;
}

// The private-literal rule for pins (docs/76 OD12, revising docs/40 D3): a
// certificate hash only ever rides with a relay named by a private-range IP
// literal, so a link can never pin a self-signed relay on the public
// internet. `relay` is a normalized origin.
export function isPinnableRelay(relay: string): boolean {
  try {
    return isPrivateIpLiteral(new URL(relay).hostname);
  } catch {
    return false;
  }
}

function parseQuery(query: string): RouteQuery {
  const out: RouteQuery = { relay: null, certHashes: [], droppedParams: [] };
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
  const certHash = params.get('certHash');
  if (certHash !== null) {
    const hashes = parseCertHashList(certHash);
    if (hashes !== null && out.relay !== null && isPinnableRelay(out.relay)) {
      out.certHashes = hashes;
    } else {
      out.droppedParams.push('certHash');
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

// The broadcast route's `?room=` code, kept as typed like a room link's. A
// malformed one is ignored: the link still opens the broadcast page.
function parseRoomParam(query: string): string | null {
  if (query === '') return null;
  try {
    const v = new URLSearchParams(query).get('room');
    return v !== null && isValidRoomCode(v) ? v : null;
  } catch {
    return null;
  }
}

// The `?nick=` prefill, sanitized like a typed nickname (whitespace
// collapsed, bounded to the wire limit). Blank ⇒ absent.
function parseNickParam(query: string): string | null {
  if (query === '') return null;
  try {
    const v = new URLSearchParams(query).get('nick');
    if (v === null) return null;
    const clean = sanitizeNickname(v);
    return clean === '' ? null : clean;
  } catch {
    return null;
  }
}

// `?desktop=1` on the broadcast route. Exactly "1": anything else is an
// unknown value and ignored, like any unusable parameter.
function parseDesktopParam(query: string): boolean {
  if (query === '') return false;
  try {
    return new URLSearchParams(query).get('desktop') === '1';
  } catch {
    return false;
  }
}

// Strip a one-shot parameter from a hash, keeping the path and the other
// parameters.
export function hashWithoutParam(hash: string, name: string): string {
  const qIndex = hash.indexOf('?');
  if (qIndex === -1) return hash;
  const params = new URLSearchParams(hash.slice(qIndex + 1));
  params.delete(name);
  const rest = params.toString();
  return rest === '' ? hash.slice(0, qIndex) : `${hash.slice(0, qIndex)}?${rest}`;
}

export function hashWithoutGrant(hash: string): string {
  return hashWithoutParam(hash, 'rt');
}

export function parseRoute(hash: string): Route {
  // Split any query off before path matching — `#/view/AB2CD3?relay=…` must
  // match exactly like `#/view/AB2CD3`.
  const raw = hash.replace(/^#/, '');
  const qIndex = raw.indexOf('?');
  const query = qIndex === -1 ? '' : raw.slice(qIndex + 1);
  // Normalize: strip a leading '/', collapse trailing '/'.
  const path = (qIndex === -1 ? raw : raw.slice(0, qIndex)).replace(/^\//, '').replace(/\/+$/, '');

  if (path === '') return { view: 'landing', ...parseQuery(query) };
  if (path === 'broadcast') {
    return {
      view: 'broadcaster',
      room: parseRoomParam(query),
      nick: parseNickParam(query),
      desktop: parseDesktopParam(query),
      ...parseQuery(query),
    };
  }
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
      return { view: 'room', code, grant: parseGrant(query), nick: parseNickParam(query), ...parseQuery(query) };
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
