// R33 (docs/83 D2, OD3, §5.7): the OBS stream key, `gawk1_<ID>_<token>`, and
// where this browser keeps it.
//
// The key is a password: anyone holding it can stream on its code. It lives
// in localStorage only (per server, beside that server's saved publish
// secret in `gawk.servers`), is sent only in the status poll's Authorization
// header, and never enters a URL, a log line, telemetry or the diagnostics
// copy (U5).
import { BROADCAST_ID_ALPHABET, RESUME_TOKEN_SIZE } from '../../transport/wire';
import { normalizeRelayOrigin } from '../../lib/relayUrl';
import { readStored, writeStored } from '../../lib/storage';

export const OBS_KEY_PREFIX = 'gawk1_';

export interface ObsKey {
  // The whole key, as OBS's Bearer Token field takes it.
  key: string;
  // The 6-character broadcast code viewers join on.
  broadcastId: string;
  // The resume token as 32 lowercase hex characters.
  tokenHex: string;
}

const TOKEN_HEX_LEN = RESUME_TOKEN_SIZE * 2;
const KEY_RE = new RegExp(`^${OBS_KEY_PREFIX}([${BROADCAST_ID_ALPHABET}]{6})_([0-9a-f]{${TOKEN_HEX_LEN}})$`);

// The key for a minted ID and token, or null when either is malformed (a
// token that isn't 16 bytes would make a key the gateway refuses).
export function buildObsKey(broadcastId: string, tokenHex: string): ObsKey | null {
  return parseObsKey(`${OBS_KEY_PREFIX}${broadcastId}_${tokenHex}`);
}

// D2's format, checked only: whether the gateway knows the key is its answer
// (an unknown well-formed key reads `idle`). Surrounding whitespace from a
// paste is dropped and the code is uppercased, as the gateway normalizes it,
// so the canonical key is what is kept and presented; the token must be
// lowercase hex, as the gateway requires.
export function parseObsKey(raw: string): ObsKey | null {
  const trimmed = raw.trim();
  if (!trimmed.startsWith(OBS_KEY_PREFIX)) return null;
  const rest = trimmed.slice(OBS_KEY_PREFIX.length);
  const sep = rest.indexOf('_');
  if (sep === -1) return null;
  const key = `${OBS_KEY_PREFIX}${rest.slice(0, sep).toUpperCase()}${rest.slice(sep)}`;
  const m = KEY_RE.exec(key);
  if (!m) return null;
  return { key, broadcastId: m[1], tokenHex: m[2] };
}

// What the page shows while the key is hidden: the code, never the token.
export function maskObsKey(k: ObsKey): string {
  return `${OBS_KEY_PREFIX}${k.broadcastId}_••••`;
}

// Per server: one key for each relay origin. The OBS page only ever uses the
// deployment's own relay (the gateway publishes nowhere else, OD4), but the
// key belongs to the relay that minted it, so a chart-side relayUrl change
// never shows the old relay's key, as the default's saved publish secret is
// pruned on the same change.
const LS_PREFIX = 'gawk.obsKey:';

function storageKey(serverUrl: string): string | null {
  const origin = normalizeRelayOrigin(serverUrl);
  return origin === null ? null : `${LS_PREFIX}${origin}`;
}

export function loadObsKey(serverUrl: string): ObsKey | null {
  const k = storageKey(serverUrl);
  if (k === null) return null;
  const raw = readStored(k);
  return raw === null ? null : parseObsKey(raw);
}

export function saveObsKey(serverUrl: string, key: ObsKey): void {
  const k = storageKey(serverUrl);
  if (k !== null) writeStored(k, key.key);
}

export function forgetObsKey(serverUrl: string): void {
  const k = storageKey(serverUrl);
  if (k !== null) writeStored(k, null);
}
