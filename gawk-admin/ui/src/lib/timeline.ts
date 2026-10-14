// The moderation timeline's pure rules (R70, docs/72): how a pattern reads,
// which incident the panel opens on, where an incident or a tick sits on the
// axis. Policy — what counts as sustained, what is suggested — is computed by
// gawk-admin (D8) and only rendered here.

import type { ContentFlag, Timeline, TimelinePattern, TimelineSession } from '../api/types.ts';

/** How each pattern reads in the summary tile, and its tone. */
export const PATTERN_COPY: Record<TimelinePattern, { label: string; detail: string; tone: 'bad' | 'warn' | 'plain' }> = {
  none: { label: 'No signal', detail: 'Nothing reached the threshold', tone: 'plain' },
  isolated: { label: 'Isolated', detail: 'A short signal — often a game scene', tone: 'plain' },
  flicker: { label: 'Flicker', detail: 'Scores hovering around the threshold', tone: 'warn' },
  sustained: { label: 'Sustained', detail: 'A long run above the threshold', tone: 'bad' },
  recurring: { label: 'Recurring', detail: 'The signal keeps coming back', tone: 'bad' },
  unknown: { label: 'Unknown', detail: 'The sampler reported no threshold', tone: 'plain' },
};

/** Why a dismissal is not a violation (docs/72 D5), in the panel's words. */
export const DISMISS_REASONS = [
  { value: 'game_scene', label: 'Game scene' },
  { value: 'not_explicit', label: 'Not explicit' },
  { value: 'detector_error', label: 'Detector error' },
  { value: 'other', label: 'Other' },
] as const;

/** How long a dismissal mutes re-flagging (docs/72 D5). */
export const MUTE_SCOPES = [
  { value: 'episode', label: 'Until this episode clears', hint: 'A new episode later still flags' },
  { value: 'duration', label: 'The mute period', hint: "The deployment's -content-flag-mute" },
  { value: 'session', label: 'Rest of this session', hint: 'Ends when the publisher reconnects or stops' },
] as const;

/** The Violation step's ban lengths; `null` seconds is permanent. */
export interface BanChoice {
  id: 'cooldown' | 'day' | 'week' | 'permanent';
  label: string;
  seconds: number | null;
}

export const BAN_CHOICES: readonly BanChoice[] = [
  { id: 'cooldown', label: 'Just end it', seconds: 0 },
  { id: 'day', label: '24 hours', seconds: 86_400 },
  { id: 'week', label: '7 days', seconds: 604_800 },
  { id: 'permanent', label: 'Permanently', seconds: null },
];

/** The ban choice the server's suggestion points at, or `cooldown`. */
export function suggestedBan(timeline: Pick<Timeline, 'suggestion'>): BanChoice['id'] {
  const s = timeline.suggestion.banSeconds;
  if (!s) return 'cooldown';
  return BAN_CHOICES.find((c) => c.seconds === s)?.id ?? 'cooldown';
}

/**
 * The incident the panel opens on: the newest unresolved flag-mode one — the
 * one that needs a person — else the newest of any kind, else none.
 */
export function defaultIncident(incidents: readonly ContentFlag[]): ContentFlag | null {
  const newest = [...incidents].sort((a, b) => Date.parse(b.raisedAt) - Date.parse(a.raisedAt));
  return newest.find((f) => f.mode === 'flag' && f.state !== 'resolved') ?? newest[0] ?? null;
}

/** When an incident's bar ends on the axis: resolution, a held clear, or now. */
export function incidentEnd(f: ContentFlag, to: number): number {
  const end = f.resolvedAt ?? f.clearedAt ?? null;
  const t = end ? Date.parse(end) : to;
  return Number.isFinite(t) ? Math.min(t, to) : to;
}

/** A position on the axis, 0–100, clamped. */
export function axisPct(t: number, from: number, to: number): number {
  if (!(to > from)) return 0;
  return Math.max(0, Math.min(100, ((t - from) / (to - from)) * 100));
}

/** Ticks on round local times, about five across. */
export function axisTicks(from: number, to: number): { at: number; label: string }[] {
  const span = to - from;
  const steps = [60_000, 2 * 60_000, 5 * 60_000, 10 * 60_000, 15 * 60_000, 30 * 60_000, 3_600_000, 2 * 3_600_000, 6 * 3_600_000];
  const step = steps.find((s) => span / s <= 6) ?? steps[steps.length - 1];
  const offset = new Date(from).getTimezoneOffset() * 60_000;
  const out: { at: number; label: string }[] = [];
  for (let t = Math.ceil((from - offset) / step) * step + offset; t <= to; t += step) {
    out.push({
      at: t,
      label: new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }),
    });
  }
  return out;
}

/** The session a moment falls in, if any. */
export function sessionAt(sessions: readonly TimelineSession[], t: number): TimelineSession | null {
  return (
    sessions.find((s) => Date.parse(s.firstSampleAt) <= t && t <= Date.parse(s.lastSampleAt) + 5_000) ?? null
  );
}

/** The threshold in force at a moment: its session's, else the newest known. */
export function thresholdAt(timeline: Pick<Timeline, 'sessions' | 'levels'>, t: number): number | null {
  return sessionAt(timeline.sessions, t)?.threshold ?? timeline.levels.threshold ?? null;
}

/** `41 min`, `3 min`, `40 s` — the tiles' durations. */
export function formatSeconds(s: number): string {
  if (!Number.isFinite(s) || s <= 0) return '0 s';
  if (s < 60) return `${Math.round(s)} s`;
  if (s < 3600) return `${Math.round(s / 60)} min`;
  const h = Math.floor(s / 3600);
  const m = Math.round((s % 3600) / 60);
  return m ? `${h} h ${m} min` : `${h} h`;
}

/** The label without its `_EXPOSED` suffix, which every flagged label carries. */
export function shortLabel(label: string): string {
  return label.replace(/_EXPOSED$/, '');
}
