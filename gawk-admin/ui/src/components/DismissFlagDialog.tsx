import type { ContentFlag } from '../api/types.ts';
import { Dialog } from './Dialog.tsx';
import ui from '../styles/ui.module.css';

/**
 * Confirm a dismissal (R40, docs/71 D10, OD11).
 *
 * A dialog rather than a one-tap button because dismissing is not "seen it":
 * it resolves the flag AND mutes re-flagging of the broadcast for the
 * deployment's mute period, during which a still-explicit stream raises
 * nothing. Acknowledge is the one-tap "I'm looking"; this is "I looked, and
 * it is fine".
 */
export function DismissFlagDialog({
  flag,
  busy = false,
  error = null,
  onCancel,
  onConfirm,
}: {
  flag: ContentFlag;
  busy?: boolean;
  error?: string | null;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog title={`Dismiss content flag #${flag.id}`} busy={busy} onCancel={onCancel}>
      <p className={ui.sub}>
        Resolves the flag on <code className={ui.mono}>{flag.broadcastId}</code> as a false alarm
        and mutes re-flagging of that broadcast for the configured mute period (30 minutes by
        default). If the signal persists after the mute expires, a new flag is raised and pages
        again.
      </p>
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}
      <div className={ui.actions}>
        <button type="button" className={ui.primary} disabled={busy} onClick={onConfirm}>
          Dismiss and mute
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </Dialog>
  );
}
