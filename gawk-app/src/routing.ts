// Pure hash-route parsing for the production UI (docs/10 Decision 1). Three
// production surfaces plus the frozen debug tree. Kept pure and unit-tested;
// App.tsx subscribes to hashchange and renders the result.
import { isValidBroadcastId } from './lib/broadcastId';

export type Route =
  | { view: 'landing' }
  | { view: 'broadcaster' }
  | { view: 'viewer'; broadcastId: string }
  | { view: 'terms' }
  | { view: 'debug-index' }
  | { view: 'debug-broadcast' }
  | { view: 'debug-view' }
  | { view: 'debug-loopback' }
  // #/view with no/invalid id, or anything unknown, sends the user to the
  // landing page (which owns code entry).
  | { view: 'redirect'; to: string };

export const HOME = '#/';

export function parseRoute(hash: string): Route {
  // Normalize: strip a leading '#', then a leading '/', collapse trailing '/'.
  const path = hash.replace(/^#/, '').replace(/^\//, '').replace(/\/+$/, '');

  if (path === '') return { view: 'landing' };
  if (path === 'broadcast') return { view: 'broadcaster' };
  // R23 (docs/29): the terms surface, reachable from every surface, gated
  // behind nothing.
  if (path === 'terms') return { view: 'terms' };

  if (path === 'view' || path.startsWith('view/')) {
    const id = path.slice('view/'.length).toUpperCase();
    if (path !== 'view' && isValidBroadcastId(id)) {
      return { view: 'viewer', broadcastId: id };
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
