// The `?nick=` prefill on a room link or a broadcast link.
//
// A page outside the app (the Mumble bot's room card) can hand over the
// name the user goes by there. It only PREFILLS: the room view still shows
// the nickname prompt, with the link's name in the field, even over a name
// this browser remembers — a forwarded link carries someone else's name, so
// joining under it unasked would be wrong. Confirming it remembers it like
// any typed nickname (roomPrefs.ts).
//
// App.tsx renders the screen with the route's `nick` and strips the
// parameter before the first render, so a copied link does not pass the
// name on. The screen reads it once, on mount; a reload no longer has it,
// which costs nothing once the name was confirmed (it is remembered).
import { hashWithoutParam, type Route } from '../../routing';

export function applyRouteNick(route: Route): void {
  if (route.view !== 'room' && route.view !== 'broadcaster') return;
  if (typeof window === 'undefined') return;
  const cleaned = hashWithoutParam(window.location.hash, 'nick');
  if (cleaned === window.location.hash) return;
  const url = `${window.location.pathname}${window.location.search}${cleaned}`;
  try {
    window.history.replaceState(window.history.state, '', url);
  } catch {
    // A history API that refuses (sandboxed iframes) leaves the name in the
    // URL; it is only a prefill.
  }
}
