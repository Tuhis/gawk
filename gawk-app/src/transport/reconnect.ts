// Shared reconnect policy (R17 W1/W2, docs/22 Decision 4) — used by the
// viewer's ViewerSession and the broadcaster's auto-resume, so both clients
// react to a relay drain or an abrupt pod death identically.

import { CLOSE_CODE_SERVER_DRAINING } from './wire';

export const RECONNECT_MAX_ATTEMPTS = 10;

// The first retry after an *abrupt* session death (no close code — a crashed
// pod, a stateless reset, a flushed conntrack path). Fast because the
// replacement pod is already behind the Service; the old 1 s floor ate the
// entire rollout blip budget on its own.
export const ABRUPT_DROP_RETRY_DELAY_MS = 250;

// 1s, 2s, 4s, 8s, then capped at 15s — ~100s total before giving up,
// comfortably covering a relay restart.
//
// R17 overlays a fast first retry on top: a 4002 (server draining) means
// "reconnect now" (0 ms — the drain happens while a ready pod is behind the
// same Service), and an abrupt post-connect drop (no close code) retries in
// ABRUPT_DROP_RETRY_DELAY_MS. Only the first attempt after a connected
// session died is fast — attempt 2+ follows the ladder unchanged, and coded
// non-drain closes (4001 eviction, 429/500) keep the 1 s ladder from the
// start. closeCode is null/undefined for abrupt drops.
export function reconnectDelayMs(attempt: number, closeCode?: number | null): number {
  if (attempt === 1) {
    if (closeCode === CLOSE_CODE_SERVER_DRAINING) return 0;
    if (closeCode == null) return ABRUPT_DROP_RETRY_DELAY_MS;
  }
  return Math.min(1000 * 2 ** (attempt - 1), 15_000);
}

export interface ReconnectInfo {
  attempt: number;
  delayMs: number;
  reason: string;
  // The close code that ended the previous session, when there was one:
  // 4002 = planned relay drain (the UI shows calmer copy for it).
  closeCode?: number | null;
}
