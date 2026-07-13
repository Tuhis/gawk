// Shared broadcast-code helpers for the production UI (landing code entry,
// routing, viewer). The relay mints 6-char codes from an unambiguous alphabet
// (no 0/O/1/I/L); BROADCAST_ID_ALPHABET is the source of truth in wire.ts.
import { BROADCAST_ID_ALPHABET } from '../transport/wire';

export const BROADCAST_ID_LENGTH = 6;

// Uppercase and keep only alphabet characters, capped at the code length.
// Used by the segmented code input to filter typing and paste.
export function sanitizeBroadcastId(raw: string): string {
  let out = '';
  for (const ch of raw.toUpperCase()) {
    if (BROADCAST_ID_ALPHABET.includes(ch)) out += ch;
    if (out.length === BROADCAST_ID_LENGTH) break;
  }
  return out;
}

export function isValidBroadcastId(id: string): boolean {
  return id.length === BROADCAST_ID_LENGTH && sanitizeBroadcastId(id) === id;
}
