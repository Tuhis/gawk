// R42 (docs/44 §4.8): the one-shot `?rt=` grant hand-off on a room link.
//
// A native broadcaster's "open room view" launches the browser at
// `#/room/<code>?rt=<grant>`. The grant is a credential (a creator token or a
// static room's attach secret), so it must not sit in the address bar where
// it gets copied along with the link: App.tsx moves it into session storage,
// keyed by the room code, and rewrites the hash without it — via
// history.replaceState, so no hashchange fires and nothing re-renders —
// BEFORE the room screen mounts. Session storage on purpose: it dies with
// the tab, and a reload of the same tab still has it (the room session
// reconnects with the same grant).
//
// Grant shapes (this module is the format's one home; RM6's natives and the
// broadcaster's link field both produce it):
//   c:<hex32>   a creator token (dynamic rooms)       — also accepted bare
//   a:<secret>  a static room's attach secret
import { hashWithoutGrant, type Route } from '../../routing';

export type RoomGrant = { kind: 'creator'; tokenHex: string } | { kind: 'attach'; secret: string };

const STORAGE_PREFIX = 'gawk:room-grant:';

const HEX32 = /^[0-9a-f]{32}$/i;

// Storage key: the code normalized the way the relay normalizes it, so a link
// typed in either case finds its grant.
function storageKey(code: string): string {
  return STORAGE_PREFIX + code.toLowerCase();
}

export function parseGrant(raw: string): RoomGrant | null {
  const v = raw.trim();
  if (v === '') return null;
  if (v.startsWith('a:') || v.startsWith('a.')) {
    const secret = v.slice(2);
    return secret === '' ? null : { kind: 'attach', secret };
  }
  const hex = v.startsWith('c:') || v.startsWith('c.') ? v.slice(2) : v;
  return HEX32.test(hex) ? { kind: 'creator', tokenHex: hex.toLowerCase() } : null;
}

export function formatGrant(grant: RoomGrant): string {
  return grant.kind === 'creator' ? `c:${grant.tokenHex}` : `a:${grant.secret}`;
}

export function stashGrant(code: string, grant: RoomGrant): void {
  try {
    sessionStorage.setItem(storageKey(code), JSON.stringify(grant));
  } catch {
    // Private mode / quota: the grant lives only for this navigation.
  }
}

export function readGrant(code: string): RoomGrant | null {
  try {
    const raw = sessionStorage.getItem(storageKey(code));
    if (raw === null) return null;
    const parsed = JSON.parse(raw) as Partial<RoomGrant>;
    if (parsed.kind === 'creator' && typeof parsed.tokenHex === 'string' && HEX32.test(parsed.tokenHex)) {
      return { kind: 'creator', tokenHex: parsed.tokenHex };
    }
    if (parsed.kind === 'attach' && typeof parsed.secret === 'string' && parsed.secret !== '') {
      return { kind: 'attach', secret: parsed.secret };
    }
    return null;
  } catch {
    return null;
  }
}

export function clearGrant(code: string): void {
  try {
    sessionStorage.removeItem(storageKey(code));
  } catch {
    // nothing to clear
  }
}

// Called synchronously from App.tsx's route resolution, before the screen
// renders. A malformed grant is dropped quietly (R26 D7: never fatal) and the
// URL is still cleaned, so a junk `rt` never lingers either.
export function applyRouteGrant(route: Route): void {
  if (route.view !== 'room' || route.grant === null) return;
  const grant = parseGrant(route.grant);
  if (grant) stashGrant(route.code, grant);
  if (typeof window === 'undefined') return;
  const cleaned = hashWithoutGrant(window.location.hash);
  if (cleaned === window.location.hash) return;
  const url = `${window.location.pathname}${window.location.search}${cleaned}`;
  try {
    window.history.replaceState(window.history.state, '', url);
  } catch {
    // A history API that refuses (sandboxed iframes) leaves the grant in the
    // URL for this load; the stash still made the join work.
  }
}
