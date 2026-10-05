// "Start streaming from inside a room". The room view
// stashes what it knows here and navigates to #/broadcast; the broadcaster
// page reads it once on mount and treats it as a PENDING room: no panel, no
// prompt, no re-typed code — the broadcast joins the room by itself the
// moment it is live (the room needs the running broadcast's resume token, so
// the join cannot happen any earlier). The nickname the participant already
// answered rides along so the hop never asks twice; a guest stays a guest.
// Session storage: a reload of the broadcaster tab should not silently
// re-attach to a room the user already left.
//
// A `#/broadcast?room=<code>` link from outside the app (a chat bot's room
// card) is the same hop without a nickname: App.tsx stashes the code here and
// strips the parameter before the page mounts, so the room view asks for (or
// remembers) the nickname as it does for any join.
import { hashWithoutParam, type Route } from '../../routing';

const KEY = 'gawk:room-return';

export interface RoomReturn {
  code: string;
  // The nickname in use in the room, or null for a guest (the relay names
  // guests; the broadcaster's session gets its own guest name). Undefined
  // when nobody has been asked yet: a `?room=` link.
  nickname: string | null | undefined;
  // Where the hop came from (R67, docs/69 D5): 'link' for a `?room=` link
  // from outside the app, 'room' for the room view's own buttons. Only a
  // 'link' hop may launch the desktop app automatically. A stash without it
  // (an older tab) reads as 'room'.
  source: 'link' | 'room';
  // Set when the room view already launched the desktop app on the way
  // here: 'opening' after "…or in the desktop app", 'auto' after an
  // automatic launch from "Start streaming here". The broadcaster shows the
  // matching modal and never launches a second time.
  handoff?: 'opening' | 'auto';
}

export function stashRoomReturn(ret: RoomReturn): void {
  try {
    sessionStorage.setItem(KEY, JSON.stringify(ret));
  } catch {
    // the user can still join by code from the Room panel
  }
}

// Read-and-clear: one hop, one use.
export function takeRoomReturn(): RoomReturn | null {
  try {
    const v = sessionStorage.getItem(KEY);
    if (v !== null) sessionStorage.removeItem(KEY);
    if (v === null || v === '') return null;
    const parsed = JSON.parse(v) as Partial<RoomReturn>;
    if (typeof parsed.code !== 'string' || parsed.code === '') return null;
    const source = parsed.source === 'link' ? 'link' : 'room';
    const handoff = parsed.handoff === 'opening' || parsed.handoff === 'auto' ? parsed.handoff : undefined;
    // JSON drops an undefined nickname, so an absent key means "not asked".
    const nickname = !('nickname' in parsed)
      ? undefined
      : typeof parsed.nickname === 'string' && parsed.nickname !== ''
        ? parsed.nickname
        : null;
    return handoff === undefined ? { code: parsed.code, nickname, source } : { code: parsed.code, nickname, source, handoff };
  } catch {
    return null;
  }
}

// Called synchronously from App.tsx's route resolution, before the screen
// renders: a `?room=` on the broadcast route becomes a pending room, and the
// parameter leaves the URL so a reload does not join again.
export function applyRouteRoom(route: Route): void {
  if (route.view !== 'broadcaster' || route.room === null) return;
  stashRoomReturn({ code: route.room, nickname: undefined, source: 'link' });
  if (typeof window === 'undefined') return;
  const cleaned = hashWithoutParam(window.location.hash, 'room');
  if (cleaned === window.location.hash) return;
  const url = `${window.location.pathname}${window.location.search}${cleaned}`;
  try {
    window.history.replaceState(window.history.state, '', url);
  } catch {
    // A history API that refuses (sandboxed iframes) leaves the parameter in
    // the URL; a reload then joins the room again, which is all it costs.
  }
}
