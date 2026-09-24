// The two shapes a room code takes: six characters of the broadcast alphabet
// (a dynamic room; the relay keeps it disjoint from broadcast IDs) or a static
// room's slug of 3–32 of [A-Za-z0-9-], case-insensitive. This only says
// "well-formed enough to ask the relay about", never which kind it is.
import { isValidBroadcastId } from './broadcastId';
import { MAX_ROOM_CODE_LEN } from '../transport/wire';

export const MIN_ROOM_SLUG_LEN = 3;

const SLUG = /^[A-Za-z0-9-]+$/;

export function isValidRoomSlug(code: string): boolean {
  return code.length >= MIN_ROOM_SLUG_LEN && code.length <= MAX_ROOM_CODE_LEN && SLUG.test(code);
}

export function isValidRoomCode(code: string): boolean {
  return isValidRoomSlug(code) || isValidBroadcastId(code.toUpperCase());
}

// What the broadcaster's "room code or link" field holds: a room link or a
// bare code. Accepts `…#/room/<code>`, `#/room/<code>`, `/room/<code>` and a
// plain code; returns the code as written, or null when nothing usable is
// there. The `?rt=` grant, if any, is returned beside it so the broadcaster
// can present it exactly as the SPA route would.
export function parseRoomLink(input: string): { code: string; grant: string | null } | null {
  const raw = input.trim();
  if (raw === '') return null;
  const m = /(?:^|#|\/)room\/([^/?#\s]+)(?:\?([^#\s]*))?/.exec(raw);
  let code: string;
  let grant: string | null = null;
  if (m) {
    try {
      code = decodeURIComponent(m[1]);
    } catch {
      return null;
    }
    if (m[2]) {
      try {
        grant = new URLSearchParams(m[2]).get('rt') || null;
      } catch {
        grant = null;
      }
    }
  } else if (SLUG.test(raw)) {
    code = raw;
  } else {
    return null;
  }
  return isValidRoomCode(code) ? { code, grant } : null;
}
