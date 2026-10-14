import type { Broadcast, ContentFlag } from '../api/types.ts';
import { formatDuration, formatInstant } from '../lib/format.ts';
import { describeLabels, formatScore, isPinned } from '../lib/flags.ts';
import { timelineHref } from '../router/router.ts';
import ui from '../styles/ui.module.css';

/**
 * The red pin on a content-flagged broadcast's ID cell (R40, docs/71 §9.4).
 *
 * R39 put this slot in the row and the sort order while there was no data to
 * argue with (docs/42 §4.11, AP6); R40 gives it a source, `contentFlag`. It
 * renders only for an unresolved flag-mode flag — an observed flag pages no one
 * and pins nothing (D19) — and nothing at all otherwise, so an unflagged row
 * carries no stray gap.
 */
export function FlaggedPinSlot({ broadcast }: { broadcast: Broadcast }) {
  const flag = broadcast.contentFlag;
  if (!isPinned(flag)) return null;
  return (
    <span
      className={ui.badgeBad}
      title={`Content flag #${flag.id}: sustained explicit-content signal`}
    >
      {flag.state === 'open' ? 'flagged' : 'flagged · acknowledged'}
    </span>
  );
}

/**
 * Everything an operator needs to judge one flag at a glance — score, window,
 * age and state — beside the four things they can do about it. Scores and
 * metadata only: the sampler keeps no frames (D8), so judging means joining
 * live through Watch (OD4).
 *
 * The actions are callbacks; the view owns the requests. `onWatch` is absent
 * when the deployment has no `-app-base-url`, and `onKill` when the broadcast
 * is not in the current fleet scan.
 */
export function ContentFlagStrip({
  flag,
  now,
  busy,
  watch,
  onAcknowledge,
  onDismiss,
  onKill,
}: {
  flag: ContentFlag;
  /** The fetch time the ages are measured from (useLoader's `loadedAt`). */
  now: number;
  busy: boolean;
  watch?: string;
  onAcknowledge: () => void;
  onDismiss: () => void;
  onKill?: () => void;
}) {
  const raised = Date.parse(flag.raisedAt);
  return (
    <div>
      <div className={ui.flagFacts}>
        <strong>Content flag #{flag.id}</strong>
        {flag.state === 'open' ? (
          <span className={ui.badgeBad}>open</span>
        ) : (
          <span
            className={ui.badgeWarn}
            title={flag.acknowledgedAt ? `acknowledged ${formatInstant(flag.acknowledgedAt)}` : undefined}
          >
            acknowledged{flag.acknowledgedBy ? ` by ${flag.acknowledgedBy}` : ''}
          </span>
        )}
        <SignalBadge flag={flag} />
        <span>
          score <strong>{formatScore(flag.scores.max)}</strong>
          <span className={ui.dim}>
            {' '}
            (mean {formatScore(flag.scores.mean)}, peak {formatScore(flag.peakScore)})
          </span>
        </span>
        <span>
          window {flag.window.flaggedSamples} of {flag.window.samples}
        </span>
        <span className={ui.mono}>{describeLabels(flag)}</span>
        <span className={ui.dim} title={formatInstant(flag.raisedAt)}>
          raised {Number.isFinite(raised) ? `${formatDuration(now - raised)} ago` : '—'}
        </span>
        {flag.reminders > 0 ? (
          <span className={ui.dim}>
            re-paged {flag.reminders}×
          </span>
        ) : null}
      </div>
      <div className={ui.actions}>
        {flag.state === 'open' ? (
          <button type="button" className={ui.primary} disabled={busy} onClick={onAcknowledge}>
            Acknowledge
          </button>
        ) : null}
        <button type="button" disabled={busy} onClick={onDismiss}>
          Dismiss…
        </button>
        {watch ? (
          <a href={watch} target="_blank" rel="noreferrer">
            Watch
          </a>
        ) : null}
        {/* R70: the history behind the flag, where the decision is made. */}
        {flag.broadcastKey ? <a href={timelineHref(flag.broadcastKey)}>Timeline</a> : null}
        {onKill ? (
          <button type="button" className={ui.danger} disabled={busy} onClick={onKill}>
            Kill…
          </button>
        ) : null}
      </div>
    </div>
  );
}

/**
 * The sampler's last word (D9). `unknown` is not good news: no report for
 * longer than stale-after means the sampler went quiet, and a silent sampler is
 * not evidence of a clean broadcast — the flag keeps re-paging.
 */
function SignalBadge({ flag }: { flag: ContentFlag }) {
  if (flag.signal === 'live') return <span className={ui.badgeBad}>signal live</span>;
  if (flag.signal === 'unknown') {
    return (
      <span
        className={ui.badgeWarn}
        title="No sampler report for longer than the stale-after window. The flag keeps re-paging until someone acknowledges it."
      >
        signal unknown
      </span>
    );
  }
  return <span className={ui.badge}>signal cleared</span>;
}
