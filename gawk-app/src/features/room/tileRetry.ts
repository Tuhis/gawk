// A ViewerSession that has given up (a refused first dial, a spent reconnect
// budget) never re-dials by policy, so the tile re-creates it: when the
// roster shows its attachment live while the session is dead, and on a tap
// of Retry. Automatic retries wait TILE_RETRY_DELAYS_MS[n] (n = attempts in
// a row that never connected, capped at the last entry) and stop after
// TILE_AUTO_RETRIES in a row; an away -> live flip earns a fresh budget but
// not a shorter delay, so a flapping attachment re-dials at most about once
// a minute and a relay that keeps refusing stops being asked. Retry always
// works.
export const TILE_RETRY_DELAYS_MS = [2_000, 10_000, 30_000, 60_000] as const;
export const TILE_AUTO_RETRIES = 3;
