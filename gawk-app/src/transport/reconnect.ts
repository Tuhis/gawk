// Shared reconnect policy (R17 W1/W2, docs/22 Decision 4) — used by the
// viewer's ViewerSession and the broadcaster's auto-resume, so both clients
// react to a relay drain or an abrupt pod death identically.

import {
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_ROOM_ENDED,
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
} from './wire';

export const RECONNECT_MAX_ATTEMPTS = 10;

// Close codes after which a VIEWER must stay down. Retrying one of these is
// not merely wasted — 4006 means the broadcast was killed and its ID banned
// (R39, docs/42 §4.4), so a reconnect loop would hammer the relay's ban gate
// for the whole cooldown. A named set rather than an inline `||` because
// viewer-session.ts has to make the same judgement in two places, and the two
// drifting apart is exactly how a kill turns into a reconnect storm.
const TERMINAL_VIEWER_CLOSE_CODES: ReadonlySet<number> = new Set([
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
]);

// Close codes after which a PUBLISHER must not auto-resume. 4004 is a relay
// invariant rather than a preference — "newest publisher wins" only converges
// because the deposed session does not come back (wire.ts) — and 4006 is the
// operator's kill: the ID is banned for at least the cooldown, so a reclaim
// would only collect a 451 the browser cannot even read (docs/42 D15). Kept
// byte-identical to the natives' terminalForPublisher /
// terminal_for_publisher so all three broadcasters give up together.
const TERMINAL_PUBLISHER_CLOSE_CODES: ReadonlySet<number> = new Set([
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
]);

// Close codes after which a ROOM CONTROL session must stay down (R42,
// docs/44 §4.4): 4007 means the room ended — empty-grace expiry, a creator's
// end, or the operator deleting the CR — and the participant's media sessions
// have their own lifecycle. Every other close (a home-pod move, a drain, an
// abrupt drop) is exactly what the room reconnect exists for: the roster is
// rebuilt from live control sessions, so coming back IS the recovery.
const TERMINAL_ROOM_CLOSE_CODES: ReadonlySet<number> = new Set([CLOSE_CODE_ROOM_ENDED]);

// An abrupt drop (null/undefined close code) is never terminal — that is the
// case reconnect exists for. Both are type predicates so a caller that needs
// the code afterwards (to render its sentence) doesn't have to re-assert it.
export function isTerminalViewerClose(code: number | null | undefined): code is number {
  return code != null && TERMINAL_VIEWER_CLOSE_CODES.has(code);
}

export function isTerminalPublisherClose(code: number | null | undefined): code is number {
  return code != null && TERMINAL_PUBLISHER_CLOSE_CODES.has(code);
}

export function isTerminalRoomClose(code: number | null | undefined): code is number {
  return code != null && TERMINAL_ROOM_CLOSE_CODES.has(code);
}

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
//
// Terminal codes never reach here: callers ask isTerminalViewerClose /
// isTerminalPublisherClose first and stop. There is deliberately no "never"
// return value — a delay of Infinity would still schedule a timer.
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
