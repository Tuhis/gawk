import { useState } from 'react';

import type { Broadcast } from '../api/types.ts';
import { BAN_PRESETS, expiryFromNow } from '../lib/format.ts';
import { defaultPrefixLength, parseIp, prefixChoices, sharedIpVerdict } from '../lib/ip.ts';
import { Dialog } from './Dialog.tsx';
import ui from '../styles/ui.module.css';

export interface BanRequestDraft {
  reason: string;
  /** RFC3339, or null for a permanent ban. */
  expiresAt: string | null;
  /** null = do not ban the publisher IP. */
  ip: { prefixLength: number } | null;
}

/**
 * Kill + ban (docs/42 §4.9).
 *
 * The ID ban is the action; the IP ban is the loaded weapon, and everything
 * unusual in this dialog is about the IP half:
 *
 *   * **The prefix is offered, not assumed** — v4 `/32`, v6 `/64`. A v6 `/128`
 *     is near-useless because privacy-address rotation hands the same machine a
 *     new address whenever it likes, so `/64` is the smallest unit that
 *     corresponds to a subscriber.
 *   * **The shared-IP warning** fires when more than half the live broadcasts
 *     report the same publisher IP. That is the tell that the load balancer is
 *     not preserving client addresses, in which case an IP ban is not a
 *     targeted action but a fleet-wide outage switch. The relay cannot detect
 *     this itself (§5), so the honest place to say it is here, in front of the
 *     operator, at the moment they are about to do it.
 *   * **The NAT caveat is always on screen** whenever an IP ban is on offer —
 *     not behind the checkbox, because the point is to be read before the
 *     checkbox is ticked.
 */
export function BanDialog({
  broadcast,
  broadcasts,
  busy = false,
  error = null,
  onCancel,
  onConfirm,
}: {
  broadcast: Broadcast;
  /** The whole live fleet: the shared-IP heuristic is about the fleet, not the row. */
  broadcasts: readonly Broadcast[];
  busy?: boolean;
  error?: string | null;
  onCancel: () => void;
  onConfirm: (draft: BanRequestDraft) => void;
}) {
  const publisher = parseIp(broadcast.publisherRemoteIp);
  const [reason, setReason] = useState('');
  const [durationIndex, setDurationIndex] = useState(1); // 24 hours
  const [banIp, setBanIp] = useState(false);
  const [prefix, setPrefix] = useState(publisher ? defaultPrefixLength(publisher.family) : 32);
  const [complaint, setComplaint] = useState<string | null>(null);

  const shared = sharedIpVerdict(broadcasts, broadcast.publisherRemoteIp);
  const cidr = publisher ? `${publisher.ip}/${prefix}` : '';

  function submit() {
    if (!reason.trim()) {
      setComplaint('A reason is required — it is what the audit trail and the webhook carry.');
      return;
    }
    setComplaint(null);
    onConfirm({
      reason: reason.trim(),
      expiresAt: expiryFromNow(BAN_PRESETS[durationIndex].seconds, Date.now()),
      ip: banIp && publisher ? { prefixLength: prefix } : null,
    });
  }

  return (
    <Dialog title={`Kill and ban ${broadcast.id}`} busy={busy} onCancel={onCancel}>
      <p className={ui.sub}>
        Ends the broadcast now and refuses this ID for the chosen duration. The broadcaster's
        resume token will not bring it back.
      </p>

      <div className={ui.field}>
        <label htmlFor="ban-reason">Reason (required)</label>
        <textarea
          id="ban-reason"
          rows={3}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </div>

      <fieldset className={ui.field}>
        <legend className={ui.sub}>Duration</legend>
        <div className={ui.row}>
          {BAN_PRESETS.map((p, i) => (
            <label key={p.label} className={ui.row}>
              <input
                type="radio"
                name="ban-duration"
                checked={durationIndex === i}
                onChange={() => setDurationIndex(i)}
              />
              {p.label}
            </label>
          ))}
        </div>
      </fieldset>

      {publisher ? (
        <div className={ui.field}>
          {shared.warn ? (
            <p className={ui.warning} role="alert">
              <strong>{shared.sharing} of {shared.total} live broadcasts</strong> report this same
              publisher IP. The load balancer is not preserving client addresses —{' '}
              <code>externalTrafficPolicy</code> must be <code>Local</code> — so this is the node
              or LB address, and banning it would end every broadcast on the fleet.
            </p>
          ) : null}

          <label className={ui.row}>
            <input
              type="checkbox"
              checked={banIp}
              onChange={(e) => setBanIp(e.target.checked)}
            />
            Also ban the publisher IP <code className={ui.mono}>{publisher.ip}</code>
          </label>

          <div className={ui.row}>
            <label htmlFor="ban-prefix">Prefix</label>
            <select
              id="ban-prefix"
              value={String(prefix)}
              onChange={(e) => setPrefix(Number(e.target.value))}
              disabled={!banIp}
            >
              {prefixChoices(publisher.family).map((n) => (
                <option key={n} value={String(n)}>
                  /{n}
                </option>
              ))}
            </select>
            <span className={ui.mono}>{cidr}</span>
          </div>

          <p className={ui.caveat}>
            One address can be many people: a ban on <code>{cidr}</code> also blocks anyone sharing
            that NAT, carrier-grade NAT or campus network.
          </p>
        </div>
      ) : (
        <p className={ui.caveat}>
          No publisher IP is known for this broadcast, so no IP ban is offered.
        </p>
      )}

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
          Kill and ban
        </button>
        <button type="button" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </Dialog>
  );
}
