// R42 (docs/44 §4.8): "start streaming from inside a room". The room view
// stashes what it knows here and navigates to #/broadcast; the broadcaster
// page reads it once on mount and treats it as a PENDING room: no panel, no
// prompt, no re-typed code — the broadcast joins the room by itself the
// moment it is live (the room needs the running broadcast's resume token, so
// the join cannot happen any earlier). The nickname the participant already
// answered rides along so the hop never asks twice; a guest stays a guest.
// Session storage: a reload of the broadcaster tab should not silently
// re-attach to a room the user already left.

const KEY = 'gawk:room-return';

export interface RoomReturn {
  code: string;
  // The nickname in use in the room, or null for a guest (the relay names
  // guests; the broadcaster's session gets its own guest name).
  nickname: string | null;
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
    return {
      code: parsed.code,
      nickname: typeof parsed.nickname === 'string' && parsed.nickname !== '' ? parsed.nickname : null,
    };
  } catch {
    return null;
  }
}
