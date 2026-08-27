// Small formatters shared by the views. Nothing here is a policy decision;
// anything that is lives beside the rule it implements.

/** The ban-duration presets §4.9 names, plus permanent. */
export interface DurationPreset {
  label: string;
  /** null = permanent (the API's `expiresAt: null`). */
  seconds: number | null;
}

export const BAN_PRESETS: readonly DurationPreset[] = [
  { label: '1 hour', seconds: 3600 },
  { label: '24 hours', seconds: 86_400 },
  { label: '7 days', seconds: 604_800 },
  { label: 'permanent', seconds: null },
];

/** `-kill-cooldown`'s documented default (§4.12), used when the API omits it. */
export const DEFAULT_KILL_COOLDOWN_SECONDS = 600;

/** `1h 04m`, `12m 30s`, `41s` — compact enough for a table cell on a phone. */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m > 0) return `${m}m ${String(sec).padStart(2, '0')}s`;
  return `${sec}s`;
}

/** Uptime since an RFC3339 instant. */
export function uptime(startedAt: string, now: number): string {
  const t = Date.parse(startedAt);
  if (Number.isNaN(t)) return '—';
  return formatDuration(now - t);
}

/**
 * Time left on a ban. `permanent` for a null expiry, `expired` once it has
 * passed — an operator scanning the list needs the distinction at a glance.
 */
export function expiresIn(expiresAt: string | null | undefined, now: number): string {
  if (!expiresAt) return 'permanent';
  const t = Date.parse(expiresAt);
  if (Number.isNaN(t)) return '—';
  return t <= now ? 'expired' : formatDuration(t - now);
}

/** RFC3339 for `now + seconds`, or null for a permanent ban. */
export function expiryFromNow(seconds: number | null, now: number): string | null {
  return seconds === null ? null : new Date(now + seconds * 1000).toISOString();
}

/** Local, second-precision. Timestamps in this UI are read, not compared. */
export function formatInstant(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  return new Date(t).toLocaleString();
}
