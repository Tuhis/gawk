// R42 (docs/44 §4.8): "start streaming from inside a room". The room view
// stashes its code here and navigates to #/broadcast; the broadcaster page
// reads it once on mount, opens its Room panel prefilled, and attaches the
// broadcast to that room as soon as it is live. Session storage: a reload
// of the broadcaster tab should not silently re-attach to a room the user
// already left.

const KEY = 'gawk:room-return';

export function stashRoomReturn(code: string): void {
  try {
    sessionStorage.setItem(KEY, code);
  } catch {
    // the user can still join by code from the Room panel
  }
}

// Read-and-clear: one hop, one use.
export function takeRoomReturn(): string | null {
  try {
    const v = sessionStorage.getItem(KEY);
    if (v !== null) sessionStorage.removeItem(KEY);
    return v === null || v === '' ? null : v;
  } catch {
    return null;
  }
}
