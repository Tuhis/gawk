// Shared reconnect policy — used by the viewer's ViewerSession and the
// broadcaster's auto-resume, so both clients react to a relay drain or an
// abrupt pod death identically.

import {
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_BROADCAST_FULL,
  CLOSE_CODE_BROADCASTER_STOPPED,
  CLOSE_CODE_POP_UPLINK_FULL,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_RELAY_FULL,
  CLOSE_CODE_ROOM_ENDED,
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
} from './wire';

export const RECONNECT_MAX_ATTEMPTS = 10;

// Close codes after which a VIEWER must stay down. Retrying one of these is not
// merely wasted — 4006 means the broadcast was killed and its ID banned, so a
// reconnect loop would hammer the relay's ban gate for the whole cooldown. A
// named set rather than an inline `||` because viewer-session.ts has to make
// the same judgement in two places, and the two drifting apart is exactly how a
// kill turns into a reconnect storm.
const TERMINAL_VIEWER_CLOSE_CODES: ReadonlySet<number> = new Set([
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
]);

// Close codes after which a PUBLISHER must not auto-resume. 4004 is a relay
// invariant rather than a preference — "newest publisher wins" only converges
// because the deposed session does not come back (wire.ts) — and 4006 is the
// operator's kill: the ID is banned for at least the cooldown, so a reclaim
// would only collect a 451 the browser cannot even read. Kept identical to the
// natives' terminalForPublisher / terminal_for_publisher so all three
// broadcasters give up together.
const TERMINAL_PUBLISHER_CLOSE_CODES: ReadonlySet<number> = new Set([
  CLOSE_CODE_BROADCAST_ENDED,
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
]);

// Close codes after which a ROOM CONTROL session must stay down: 4007 means the
// room ended — empty-grace expiry, a creator's end, or the operator deleting
// the CR — and the participant's media sessions have their own lifecycle. Every
// other close (a home-pod move, a drain, an abrupt drop) is exactly what the
// room reconnect exists for: the roster is rebuilt from live control sessions,
// so coming back IS the recovery.
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

// Capacity refusals: the relay answered, and the answer was "full". R74's
// 4012 PopUplinkFull (docs/76 item 28: the broadcast is at a LAN party whose
// internet already carries all the streams it may), 4018 BroadcastFull (the
// broadcast is at its viewer limit) and 4019 RelayFull (the relay is). None
// is terminal, and all are deliberately absent from
// TERMINAL_VIEWER_CLOSE_CODES: a slot frees when another viewer leaves, so
// the viewer retries on this fixed cadence for as long as it is refused, and
// the refusals never spend the reconnect budget. Each refused session
// connected before it was closed, so without the fixed cadence every refusal
// would restart the ladder at 1 s.
export const FULL_RETRY_MS = 20_000;
export const POP_UPLINK_FULL_RETRY_MS = FULL_RETRY_MS;

const CAPACITY_REFUSAL_CLOSE_CODES: ReadonlySet<number> = new Set([
  CLOSE_CODE_POP_UPLINK_FULL,
  CLOSE_CODE_BROADCAST_FULL,
  CLOSE_CODE_RELAY_FULL,
]);

export function isCapacityRefusal(code: number | null | undefined): boolean {
  return code != null && CAPACITY_REFUSAL_CLOSE_CODES.has(code);
}

// R78 (docs/79 D7): 4014 BroadcasterStopped means the broadcaster pressed
// Stop. It is NOT terminal and deliberately absent from
// TERMINAL_VIEWER_CLOSE_CODES: the viewer shows "Broadcast ended" and keeps
// dialling the same code, so a restart on it resumes playback by itself.
// A fixed cadence, not the ladder: the restart is a human action seconds to
// minutes away. The jitter spreads a big audience's redials (~200
// handshakes/s for 1000 viewers). These dials never spend the reconnect
// budget, the 4012 pattern.
export const BROADCASTER_STOPPED_RETRY_MS = 5_000;
export const BROADCASTER_STOPPED_JITTER_MS = 1_000;

export function isBroadcasterStopped(code: number | null | undefined): boolean {
  return code === CLOSE_CODE_BROADCASTER_STOPPED;
}

// One waiting redial's delay; `random` is in [0, 1), Math.random by default
// and fixed in tests.
export function broadcasterStoppedRetryMs(random: () => number = Math.random): number {
  return BROADCASTER_STOPPED_RETRY_MS + Math.floor(random() * BROADCASTER_STOPPED_JITTER_MS);
}

// The first retry after an *abrupt* session death (no close code — a crashed
// pod, a stateless reset, a flushed conntrack path). Fast because the
// replacement pod is already behind the Service; a 1 s floor would eat the
// entire rollout blip budget on its own.
export const ABRUPT_DROP_RETRY_DELAY_MS = 250;

// 1s, 2s, 4s, 8s, then capped at 15s — ~100s total before giving up,
// comfortably covering a relay restart.
//
// A fast first retry overlays that: a 4002 (server draining) means
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
  if (isCapacityRefusal(closeCode)) return FULL_RETRY_MS;
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
