import { useState } from 'react';

import type { Broadcast } from '../api/types.ts';
import { formatDuration } from '../lib/format.ts';
import { Dialog } from './Dialog.tsx';
import ui from '../styles/ui.module.css';

export interface KillRequestDraft {
  reason: string;
  cooldownSeconds: number;
}

/** Cooldown shortcuts. The pre-filled value comes from the server, not from here. */
const PRESETS: readonly { label: string; seconds: number }[] = [
  { label: '5 min', seconds: 300 },
  { label: '10 min', seconds: 600 },
  { label: '30 min', seconds: 1800 },
  { label: '1 hour', seconds: 3600 },
];

/**
 * Plain kill: end the broadcast and hold its ID for a cooldown (docs/42 §4.9,
 * D5).
 *
 * Two rules the dialog enforces rather than assumes:
 *
 *   * **A reason is required.** `POST /broadcasts/{id}/kill` rejects an empty
 *     one (§4.7), but more importantly the reason is the audit trail — the
 *     event, the Postgres row and the webhook all carry it, and "no reason
 *     given" is not an acceptable entry in a moderation log.
 *   * **The cooldown is pre-filled from the server's configured default**
 *     (`-kill-cooldown`), not from a constant in the SPA, so a deployment that
 *     tuned it sees its own number here.
 *   * **The cooldown must be positive**, which is the server's rule
 *     (`cooldownSeconds <= 0` is a 400) enforced where the operator can still
 *     do something about it. It is held as the string the operator typed
 *     rather than as a number: `Number('')` is 0, so clearing the box to type
 *     a new value used to leave a `0` sitting in it — and, if they hit Kill
 *     first, a refusal from the API at the final confirm, mid-incident.
 *
 * The confirm button stays ENABLED on an empty reason on purpose: a disabled
 * button explains nothing, and the operator is mid-incident.
 */
export function KillDialog({
  broadcast,
  defaultCooldownSeconds,
  busy = false,
  error = null,
  onCancel,
  onConfirm,
}: {
  broadcast: Broadcast;
  defaultCooldownSeconds: number;
  busy?: boolean;
  error?: string | null;
  onCancel: () => void;
  onConfirm: (draft: KillRequestDraft) => void;
}) {
  const [reason, setReason] = useState('');
  const [cooldown, setCooldown] = useState(String(defaultCooldownSeconds));
  const [complaint, setComplaint] = useState<string | null>(null);

  // An empty box is NaN here, not 0 — the whole reason the state is a string.
  const seconds = cooldown.trim() === '' ? Number.NaN : Number(cooldown);
  const cooldownOk = Number.isFinite(seconds) && seconds > 0;

  function submit() {
    if (!reason.trim()) {
      setComplaint('A reason is required — it is what the audit trail and the webhook carry.');
      return;
    }
    if (!cooldownOk) {
      setComplaint('The cooldown must be a positive number of seconds.');
      return;
    }
    setComplaint(null);
    onConfirm({ reason: reason.trim(), cooldownSeconds: seconds });
  }

  return (
    <Dialog title={`Kill broadcast ${broadcast.id}`} busy={busy} onCancel={onCancel}>
      <p className={ui.sub}>
        Every viewer sees the stream end with the moderator message, and the broadcaster cannot
        reclaim this ID until the cooldown expires.
      </p>

      <div className={ui.field}>
        <label htmlFor="kill-reason">Reason (required)</label>
        <textarea
          id="kill-reason"
          rows={3}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </div>

      <div className={ui.field}>
        <label htmlFor="kill-cooldown">Cooldown (seconds)</label>
        <div className={ui.row}>
          <input
            id="kill-cooldown"
            type="number"
            // 1, not 0: the server's floor, so the spinner cannot step to a
            // value the final confirm would be refused for.
            min={1}
            value={cooldown}
            onChange={(e) => setCooldown(e.target.value)}
          />
          <span className={ui.dim}>= {cooldownOk ? formatDuration(seconds * 1000) : '—'}</span>
        </div>
        <div className={ui.row}>
          {PRESETS.map((p) => (
            <button key={p.seconds} type="button" onClick={() => setCooldown(String(p.seconds))}>
              {p.label}
            </button>
          ))}
        </div>
      </div>

      {complaint ? (
        <p className={ui.error} role="alert">
          {complaint}
        </p>
      ) : null}
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}

      <div className={ui.actions}>
        <button type="button" className={ui.danger} onClick={submit} disabled={busy}>
          Kill broadcast
        </button>
        <button type="button" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </Dialog>
  );
}
