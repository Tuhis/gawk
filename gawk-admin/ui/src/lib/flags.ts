// Content-flag rules the views share (R40, docs/71). Policy lives server-side;
// these only decide what is pinned and how a flag reads.

import type { ContentFlag } from '../api/types.ts';

/**
 * Whether a flag pins its broadcast to the top of the fleet view: an
 * unresolved (`open` or `acknowledged`) flag-mode flag. Observed flags never
 * pin — they page no one and exist for calibration (D19).
 */
export function isPinned(flag: ContentFlag | null | undefined): flag is ContentFlag {
  return !!flag && flag.mode === 'flag' && flag.state !== 'resolved';
}

/**
 * Pin order: `open` before `acknowledged` — an unanswered page is the one that
 * needs a human — then the oldest first, since it has been waiting longest.
 */
export function comparePinned(a: ContentFlag, b: ContentFlag): number {
  if (a.state !== b.state) return a.state === 'open' ? -1 : 1;
  return Date.parse(a.raisedAt) - Date.parse(b.raisedAt);
}

/** A detector confidence, two decimals: `0.97`. */
export function formatScore(score: number | null | undefined): string {
  return typeof score === 'number' && Number.isFinite(score) ? score.toFixed(2) : '—';
}

/** `FEMALE_GENITALIA_EXPOSED ×4` — the labels that fired and how often. */
export function describeLabels(flag: Pick<ContentFlag, 'labels' | 'labelCounts'>): string {
  if (flag.labels.length === 0) return '—';
  return flag.labels
    .map((l) => {
      const n = flag.labelCounts?.[l];
      return n ? `${l} ×${n}` : l;
    })
    .join(', ');
}

const RESOLUTION_LABEL: Record<string, string> = {
  cleared: 'cleared',
  dismissed: 'dismissed',
  killed: 'killed',
  auto_killed: 'auto-killed',
  ended: 'session ended',
};

// A dismissal's reason (R70, docs/72 D5) reads as words; the automatic
// reasons are already words.
const REASON_LABEL: Record<string, string> = {
  game_scene: 'game scene',
  not_explicit: 'not explicit',
  detector_error: 'detector error',
};

/** A resolved flag's outcome in words, with its reason if any. */
export function describeResolution(flag: ContentFlag): string {
  const base = flag.resolution ? (RESOLUTION_LABEL[flag.resolution] ?? flag.resolution) : 'resolved';
  const reason = flag.resolutionReason ? (REASON_LABEL[flag.resolutionReason] ?? flag.resolutionReason) : '';
  return reason ? `${base} (${reason})` : base;
}
