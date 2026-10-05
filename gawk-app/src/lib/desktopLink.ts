// R67 (docs/69): handing a broadcast over to the desktop app.
//
// The link is R66's `gawk://broadcast?room=&nick=&relay=` (docs/68 D1),
// built byte for byte like the desktop engine's canonical `to_gawk`
// (desktopLink.test.ts restates its vectors). It never carries a grant, an
// attach secret or a publish secret (D3): there is no parameter for one.
//
// The page cannot tell whether the app is installed (docs/69 §3), so nothing
// here guesses: a launch is fired and forgotten, and the screen always keeps
// the browser path underneath.
import { desktopHandoffEnabled } from '../config';
import { hashWithoutParam, type Route } from '../routing';
import { detectClientIdentity } from '../transport/client-identity';
import { readStored, writeStored } from './storage';

export interface DesktopBroadcastLink {
  // A valid room code, as typed.
  room: string | null;
  // A sanitized nickname; blank counts as absent.
  nick: string | null;
  // The normalized origin of a relay other than the default fleet. Absent
  // means the default fleet, never "whatever the app has selected" (D3).
  relay: string | null;
}

// The desktop engine's encoder escapes everything but [A-Za-z0-9-_.!~*'()],
// which is exactly encodeURIComponent's set.
export function buildDesktopBroadcastLink({ room, nick, relay }: DesktopBroadcastLink): string {
  const parts: string[] = [];
  if (room !== null && room !== '') parts.push(`room=${encodeURIComponent(room)}`);
  if (nick !== null && nick !== '') parts.push(`nick=${encodeURIComponent(nick)}`);
  if (relay !== null && relay !== '') parts.push(`relay=${encodeURIComponent(relay)}`);
  return parts.length === 0 ? 'gawk://broadcast' : `gawk://broadcast?${parts.join('&')}`;
}

// D2: the desktop apps exist for Windows, macOS and Linux. This is
// presentation, not capability — a wrong guess costs a link that does
// nothing, and the modal covers that. iPadOS Safari sends a desktop
// `Macintosh` UA by default, and on an iPad a `gawk://broadcast` link is not
// harmless (the iOS app registers the scheme), so a Mac with touch points is
// taken for an iPad.
export function isDesktopForHandoff(userAgent: string, maxTouchPoints: number): boolean {
  const { os } = detectClientIdentity(userAgent);
  if (os === 'windows' || os === 'linux') return true;
  return os === 'macos' && maxTouchPoints <= 1;
}

// Whether this page offers the desktop app at all: the operator's switch
// (D7) and this device (D2).
export function desktopHandoffOffered(): boolean {
  if (!desktopHandoffEnabled()) return false;
  if (typeof navigator === 'undefined') return false;
  return isDesktopForHandoff(navigator.userAgent, navigator.maxTouchPoints ?? 0);
}

// How long the launch iframe stays in the document. The browser has handed
// the link to the OS (or given up) long before this.
const LAUNCH_FRAME_MS = 5000;

// A launch with no click behind it (D5's automatic mode, D9's `?desktop=1`).
// A hidden iframe, not `location.href`: with no handler registered, some
// browsers replace a top-level page with an error page, while an iframe's
// failure stays inside the iframe. A click on the offer's anchor needs none
// of this; its own navigation is the launch.
export function launchDesktopLink(href: string): void {
  if (typeof document === 'undefined') return;
  const frame = document.createElement('iframe');
  frame.setAttribute('aria-hidden', 'true');
  frame.tabIndex = -1;
  frame.style.display = 'none';
  frame.src = href;
  document.body.appendChild(frame);
  setTimeout(() => frame.remove(), LAUNCH_FRAME_MS);
}

// D5: the remembered "always". Absent ⇒ offer only; there is no stored
// "never". Storage that refuses means no automatic mode.
const AUTO_KEY = 'gawk:desktop-handoff';

export function loadHandoffAuto(): boolean {
  return readStored(AUTO_KEY) === 'auto';
}

export function saveHandoffAuto(on: boolean): void {
  writeStored(AUTO_KEY, on ? 'auto' : null);
}

// D5: at most one automatic launch per room in this tab. Returns true, and
// marks the room, when this is the first; false when it already launched or
// session storage refuses (no flag ⇒ no way to keep the promise ⇒ no launch).
const DONE_PREFIX = 'gawk:handoff-done:';

export function claimAutomaticHandoff(code: string): boolean {
  const key = DONE_PREFIX + code.toLowerCase();
  try {
    if (sessionStorage.getItem(key) !== null) return false;
    sessionStorage.setItem(key, '1');
    return true;
  } catch {
    return false;
  }
}

// D9: `?desktop=1` leaves the URL before the first render, like `?nick=`,
// so a reload or a copied link doesn't launch again. The screen gets it as a
// prop from the route.
export function applyRouteDesktop(route: Route): void {
  if (route.view !== 'broadcaster') return;
  if (typeof window === 'undefined') return;
  const cleaned = hashWithoutParam(window.location.hash, 'desktop');
  if (cleaned === window.location.hash) return;
  const url = `${window.location.pathname}${window.location.search}${cleaned}`;
  try {
    window.history.replaceState(window.history.state, '', url);
  } catch {
    // A history API that refuses (sandboxed iframes) leaves the parameter;
    // a reload then asks for the app again, which is all it costs.
  }
}
