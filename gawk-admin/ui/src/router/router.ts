// Hash routing for the portal (docs/42 §4.9), telemetry's tiny-router pattern.
//
// Hash routes, not the History API, and that is load-bearing rather than a
// habit: the embed server's SPA fallback assumes it, the server never sees a
// fragment, and the same deep link works on `/`, on a port-forward and under an
// Ingress sub-path. It also keeps the OIDC redirect URI stable — the redirect
// URI registered at the IdP is `origin + pathname`, and every route the
// operator can be on lives after the `#`, so no route needs its own
// registration.
//
// `#/broadcasts` is the default AND the route webhook payloads point at
// (`portalUrl`, §4.10), so it must resolve from a cold, unauthenticated load.

import { useSyncExternalStore } from 'react';

export type ViewName = 'broadcasts' | 'bans' | 'events' | 'relays' | 'webhooks' | 'not-found';

export interface Route {
  view: ViewName;
  /** The raw hash path, for naming an unknown route rather than swallowing it. */
  path: string;
  /**
   * `?key=<broadcastKey>` — the pre-filled filter a webhook payload's
   * `portalUrl` carries (§4.10), so a paged operator lands on the offending
   * row rather than matching a 12-hex key against a fleet by eye. The HMAC'd
   * key, never a raw ID (D8). Empty when the hash carries none.
   */
  key: string;
}

const VIEWS: readonly ViewName[] = ['broadcasts', 'bans', 'events', 'relays', 'webhooks'];

export function parseHash(hash: string): Route {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash;
  const [pathPart = '', queryPart = ''] = raw.split('?');
  const key = new URLSearchParams(queryPart).get('key') ?? '';
  const segments = pathPart.split('/').filter(Boolean);
  if (segments.length === 0) return { view: 'broadcasts', path: pathPart, key };
  const head = segments[0] as ViewName;
  if (VIEWS.includes(head)) return { view: head, path: pathPart, key };
  // Named, not silently redirected: a link that lands nowhere should say so
  // rather than look like it worked.
  return { view: 'not-found', path: pathPart, key };
}

export function href(view: ViewName): string {
  return `#/${view}`;
}

const listeners = new Set<() => void>();

function onHashChange() {
  for (const l of listeners) l();
}

function subscribe(cb: () => void) {
  if (listeners.size === 0 && typeof window !== 'undefined') {
    window.addEventListener('hashchange', onHashChange);
  }
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
    if (listeners.size === 0 && typeof window !== 'undefined') {
      window.removeEventListener('hashchange', onHashChange);
    }
  };
}

/**
 * Read `location.hash` on every render — deliberately, rather than caching it
 * and updating the cache from `hashchange`.
 *
 * Not every hash change announces itself. The OIDC callback lands on
 * `origin + pathname` with **no** fragment (a redirect URI cannot carry one),
 * and `session.ts` restores the route the operator asked for with
 * `history.replaceState` — which fires no `hashchange`. A cache seeded at
 * module load therefore held the callback's empty hash for the life of the
 * page: `#/relays` rendered Broadcasts, and the Relays nav link was then dead
 * too, because its href already matched `location.hash` and clicking it fired
 * no event either. That is every deep link, including the `portalUrl` in a
 * webhook (§4.10).
 *
 * Safe with `useSyncExternalStore`: the snapshot is a string, compared with
 * `Object.is`, so an unchanged hash is an unchanged snapshot.
 */
function getSnapshot(): string {
  return typeof window === 'undefined' ? '' : window.location.hash;
}

/** The current route. Re-renders on back/forward and on any navigation. */
export function useRoute(): Route {
  return parseHash(useSyncExternalStore(subscribe, getSnapshot, getSnapshot));
}
