// R42 (docs/44 §4.1): the two shapes a room code takes. A dynamic room's
// code is six characters of the broadcast alphabet (the relay guarantees the
// two namespaces are disjoint — D3); a static room's is a slug of 3–32
// characters from [A-Za-z0-9-], case-insensitive. Every six-character
// broadcast-shaped code is also a valid slug, which is fine: this only says
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

// A room link pasted into the broadcaster's "use a room link" field — or a
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
