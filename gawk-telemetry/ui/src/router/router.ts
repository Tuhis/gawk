// TH1: routing, permalinks and URL state (docs/36 UD6).
//
// **This is a correctness fix, not a feature.** `readapi.Diagnose` has always
// set `DashboardURL = base + "#/session/<id>"`, and the SPA had no router — so
// every one of those links landed on the fleet page. Those URLs are serialized
// into the `verdict` blob of every rollup row, and **rollups are permanent**:
// the defect was being written into the one artifact that is never pruned. So
// the route table below is not a design choice about navigation; `#/session/`
// is a shape that already exists in stored data and has to resolve.
//
// A hash router rather than the History API, matching gawk-app's convention:
// the dashboard's SPA fallback already assumes it, the server never sees a
// fragment, and a deep link works identically on `/`, on a port-forward and
// under an Ingress sub-path — the same three deployments the relative API
// paths exist for.
//
// **View state lives in the URL, not in a store the address bar knows nothing
// about.** Time range, filters, selected sessions, chosen metrics: all of it,
// because "send me the link" (Q10) is a real operator move and a page that
// cannot answer it forces a screenshot instead.

import { useCallback, useEffect, useSyncExternalStore } from 'react';

export type ViewName =
  | 'live'
  | 'session'
  | 'broadcast'
  | 'history'
  | 'explore'
  | 'fleet'
  | 'rules'
  | 'sql'
  | 'not-found';

export interface Route {
  view: ViewName;
  /** The path segment after the view name: a session id or a broadcast key. */
  id?: string;
  /** Query state, from the part after `?` INSIDE the hash. */
  params: URLSearchParams;
  /** The raw hash, for round-tripping. */
  raw: string;
}

const SESSION_ID = /^[0-9a-f]{24}$/;
const BROADCAST_KEY = /^[0-9a-f]{12}$/;

/**
 * Parse a hash into a route.
 *
 * Deliberately tolerant at the edges and strict about identity: `#/session/xyz`
 * resolves to the session view with a malformed id, so the view can render "no
 * such session" (TH1's criterion) rather than the router silently falling
 * through to the fleet page — which is exactly the failure this chunk exists to
 * fix, just relocated.
 */
export function parseHash(hash: string): Route {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash;
  const [pathPart, queryPart = ''] = raw.split('?');
  const params = new URLSearchParams(queryPart);
  const segments = pathPart.split('/').filter(Boolean);

  if (segments.length === 0) return { view: 'live', params, raw };
  const [head, second] = segments;
  switch (head) {
    case 'session':
      return { view: 'session', id: second ?? '', params, raw };
    case 'broadcast':
      return { view: 'broadcast', id: second ?? '', params, raw };
    case 'history':
    case 'explore':
    case 'fleet':
    case 'rules':
    case 'sql':
      return { view: head, params, raw };
    default:
      return { view: 'not-found', id: pathPart, params, raw };
  }
}

export function isSessionId(id: string | undefined): boolean {
  return !!id && SESSION_ID.test(id);
}

export function isBroadcastKey(key: string | undefined): boolean {
  return !!key && BROADCAST_KEY.test(key);
}

/** Build a hash URL from a view, an id and query state. */
export function href(
  view: ViewName,
  id?: string,
  params?: Record<string, string | number | boolean | undefined>,
): string {
  const path = view === 'live' ? '/' : id ? `/${view}/${id}` : `/${view}`;
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === '' || v === false) continue;
    q.set(k, String(v));
  }
  const s = q.toString();
  return `#${path}${s ? `?${s}` : ''}`;
}

// --- the store --------------------------------------------------------------

const listeners = new Set<() => void>();
let current = typeof window === 'undefined' ? '' : window.location.hash;

function onHashChange() {
  current = window.location.hash;
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

function getSnapshot() {
  return current;
}

/** The current route. Re-renders on back/forward and on any navigation. */
export function useRoute(): Route {
  const hash = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
  return parseHash(hash);
}

/**
 * Navigate. `replace` swaps the entry instead of pushing one — used for state
 * that changes as you drag a slider, which must not fill the back stack with
 * intermediate positions a human never chose to visit.
 */
export function navigate(to: string, replace = false) {
  if (typeof window === 'undefined') return;
  if (window.location.hash === to) return;
  if (replace) {
    const url = window.location.href.split('#')[0] + to;
    window.history.replaceState(null, '', url);
    onHashChange();
    return;
  }
  window.location.hash = to;
}

/**
 * Read and write one piece of URL state.
 *
 * Every view uses this rather than a local `useState` for anything a link
 * should carry. The rule of thumb: if reopening the URL without it would show
 * a different screen, it belongs here.
 */
export function useUrlState(
  key: string,
  fallback = '',
): [string, (v: string, replace?: boolean) => void] {
  const route = useRoute();
  const value = route.params.get(key) ?? fallback;
  const set = useCallback(
    (v: string, replace = false) => {
      const next = parseHash(window.location.hash);
      if (v === '' || v === fallback) next.params.delete(key);
      else next.params.set(key, v);
      const [path] = next.raw.split('?');
      const q = next.params.toString();
      navigate(`#${path || '/'}${q ? `?${q}` : ''}`, replace);
    },
    [key, fallback],
  );
  return [value, set];
}

/**
 * Set the document title from the route, so a pinned tab and a browser-history
 * entry say which session they are.
 */
export function useDocumentTitle(title: string) {
  useEffect(() => {
    const previous = document.title;
    document.title = title;
    return () => {
      document.title = previous;
    };
  }, [title]);
}
