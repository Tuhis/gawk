import { useState } from 'react';

import { AuthRedirect } from '@gawk/oidc-session';
import { useApi } from '../auth/AuthContext.tsx';
import { ApiError } from '../api/client.ts';
import type { Broadcast, ContentFlag, DismissReason, MuteScope, Timeline } from '../api/types.ts';
import { describeLabels, describeResolution, formatScore } from '../lib/flags.ts';
import { expiryFromNow, formatDuration, formatInstant } from '../lib/format.ts';
import { defaultPrefixLength, parseIp, prefixChoices, sharedIpVerdict } from '../lib/ip.ts';
import { killAndBan } from '../lib/moderate.ts';
import { BAN_CHOICES, DISMISS_REASONS, MUTE_SCOPES, suggestedBan } from '../lib/timeline.ts';
import type { BanChoice } from '../lib/timeline.ts';
import ui from '../styles/ui.module.css';
import styles from '../views/BroadcastTimelineView.module.css';

type Step = 'decide' | 'fine' | 'violation';

/**
 * One incident's decision (R70, docs/72 OD2, OD3): acknowledge and dismiss act
 * on the INCIDENT; ending and banning act on the BROADCASTER and are reached
 * only through Violation, which carries the ban length inside one form. The
 * steps are Look (open: Watch & acknowledge, or Acknowledge only) and Decide
 * (acknowledged: Not a violation, or Violation). Resolved and observed
 * incidents are read-only.
 *
 * Keyed by the flag's ID by its parent, so a different incident starts back at
 * its own first step.
 */
export function DecisionPanel({
  flag,
  timeline,
  broadcast,
  broadcasts,
  killCooldownSeconds,
  now,
  onChanged,
}: {
  flag: ContentFlag;
  timeline: Timeline;
  /** The live broadcast row, when the fleet scan sees it: the IP ban needs it. */
  broadcast: Broadcast | null;
  /** The whole live fleet, for the shared-IP warning (docs/42 §4.9). */
  broadcasts: readonly Broadcast[];
  killCooldownSeconds: number;
  now: number;
  /** After an action: the confirmation line, and a cue to re-read. */
  onChanged: (note: string | null, warning: string | null) => void;
}) {
  const api = useApi();
  const [step, setStep] = useState<Step>('decide');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reason, setReason] = useState<DismissReason>('game_scene');
  const [scope, setScope] = useState<MuteScope>('episode');
  const [ban, setBan] = useState<BanChoice['id']>(() => suggestedBan(timeline));
  const [banReason, setBanReason] = useState(`Explicit content — content flag #${flag.id}`);
  const publisher = parseIp(broadcast?.publisherRemoteIp);
  const [banIp, setBanIp] = useState(false);
  const [prefix, setPrefix] = useState(publisher ? defaultPrefixLength(publisher.family) : 32);

  const watch = flag.links?.watch ?? timeline.links?.watch ?? null;
  const suggested = timeline.suggestion.verdict ?? null;

  async function run(what: () => Promise<{ note: string | null; warning?: string | null } | null>) {
    setBusy(true);
    setError(null);
    try {
      const out = await what();
      if (out) onChanged(out.note, out.warning ?? null);
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      if (err instanceof ApiError && err.status === 409) {
        setError(`Flag #${flag.id} changed before your action landed (${err.message}); the timeline has been refreshed.`);
        onChanged(null, null);
      } else {
        setError(err instanceof Error ? err.message : String(err));
      }
    } finally {
      setBusy(false);
    }
  }

  const acknowledge = () =>
    run(async () => {
      await api.acknowledgeContentFlag(flag.id);
      return { note: `Acknowledged flag #${flag.id}. Re-paging stopped.` };
    });

  const dismiss = () =>
    run(async () => {
      await api.dismissContentFlag(flag.id, { reason, scope });
      return { note: `Dismissed flag #${flag.id}.` };
    });

  const violation = () =>
    run(async () => {
      const choice = BAN_CHOICES.find((c) => c.id === ban) ?? BAN_CHOICES[0];
      const why = banReason.trim();
      if (!why) {
        setError('A reason is required: it is kept in the audit log.');
        return null;
      }
      if (choice.id === 'cooldown') {
        await api.kill(flag.broadcastId, { reason: why, cooldownSeconds: killCooldownSeconds });
        return { note: `Ended ${flag.broadcastId}.` };
      }
      const out = await killAndBan(api, flag.broadcastId, {
        reason: why,
        expiresAt: expiryFromNow(choice.seconds, Date.now()),
        ip: banIp && publisher ? { prefixLength: prefix } : null,
      });
      if (out.failure !== null) {
        setError(out.failure);
        onChanged(null, out.pending);
        return null;
      }
      return { note: out.pending ? null : out.summary, warning: out.pending };
    });

  const head = (
    <div className={styles.panelHead}>
      <div className={styles.panelTitle}>
        <h2>Incident #{flag.id}</h2>
        <StateBadge flag={flag} />
      </div>
      <p className={styles.panelSub}>
        {flag.returnedAfterReview ? <strong className={styles.returned}>Returned after review · </strong> : null}
        Raised {formatInstant(flag.raisedAt)} ({formatDuration(now - Date.parse(flag.raisedAt))} ago)
        {flag.trips && flag.trips > 1 ? ` · ${flag.trips} trips` : ''}
        {flag.reminders > 0 ? ` · re-paged ${flag.reminders}×` : ''}
      </p>
      <p className={styles.panelFacts}>
        peak <strong>{formatScore(flag.peakScore)}</strong> · {describeLabels(flag)}
      </p>
    </div>
  );

  // Observed: calibration, nothing to decide (docs/71 D19).
  if (flag.mode === 'observe') {
    return (
      <aside className={styles.panel} aria-label="Decision">
        {head}
        <div className={styles.panelBody}>
          <p className={ui.sub}>
            Observe mode: this is what would have paged. Nothing here acts on it — watch the broadcast to
            judge whether the threshold is right.
          </p>
          {watch ? <WatchLink href={watch} /> : null}
          {flag.state === 'resolved' ? <p className={ui.dim}>Resolved: {describeResolution(flag)}.</p> : null}
        </div>
      </aside>
    );
  }

  if (flag.state === 'resolved') {
    return (
      <aside className={styles.panel} aria-label="Decision">
        {head}
        <div className={styles.panelBody}>
          <dl className={styles.facts}>
            {flag.acknowledgedAt ? (
              <>
                <dt>Acknowledged</dt>
                <dd>
                  {formatInstant(flag.acknowledgedAt)}
                  {flag.acknowledgedBy ? ` by ${flag.acknowledgedBy}` : ''}
                </dd>
              </>
            ) : null}
            <dt>Resolved</dt>
            <dd>
              {describeResolution(flag)}
              {flag.resolvedAt ? ` · ${formatInstant(flag.resolvedAt)}` : ''}
              {flag.resolvedBy ? ` · ${flag.resolvedBy}` : ''}
            </dd>
            {flag.mutedUntil && flag.resolution === 'dismissed' ? (
              <>
                <dt>Muted</dt>
                <dd>
                  until {formatInstant(flag.mutedUntil)}
                  {flag.muteScope ? ` (${flag.muteScope})` : ''}
                </dd>
              </>
            ) : null}
          </dl>
          <p className={ui.dim}>Resolved incidents are read-only. Broadcast actions are in the header.</p>
        </div>
      </aside>
    );
  }

  const stepIndex = flag.state === 'open' ? 0 : 1;

  return (
    <aside className={`${styles.panel} ${flag.state === 'open' ? styles.panelUrgent : ''}`} aria-label="Decision">
      {head}
      <ol className={styles.steps} aria-label="Progress">
        {['Look', 'Decide', 'Done'].map((label, i) => (
          <li key={label} aria-current={i === stepIndex ? 'step' : undefined} className={i === stepIndex ? styles.stepOn : i < stepIndex ? styles.stepDone : styles.step}>
            {i + 1} {label}
          </li>
        ))}
      </ol>
      <div className={styles.panelBody}>
        {error ? (
          <p className={ui.error} role="alert">
            {error}
          </p>
        ) : null}

        {flag.state === 'open' ? (
          <div className={styles.stack}>
            <p className={ui.sub}>
              {flag.returnedAfterReview
                ? 'The signal came back after an earlier incident was reviewed. Look at what is on screen before deciding.'
                : 'Look at what is on screen before deciding.'}
            </p>
            {watch ? (
              <a
                className={`${styles.bigButton} ${ui.primary}`}
                href={watch}
                target="_blank"
                rel="noreferrer"
                aria-disabled={busy}
                onClick={() => void acknowledge()}
              >
                Watch &amp; acknowledge
              </a>
            ) : null}
            <button type="button" className={watch ? styles.quietButton : `${styles.bigButton} ${ui.primary}`} disabled={busy} onClick={() => void acknowledge()}>
              Acknowledge only
            </button>
            <p className={ui.dim}>Acknowledging stops the re-pages. It does not dismiss or end anything.</p>
          </div>
        ) : null}

        {flag.state === 'acknowledged' && step === 'decide' ? (
          <div className={styles.stack}>
            <p className={styles.question}>What did you see?</p>
            <button type="button" className={styles.choice} disabled={busy} onClick={() => setStep('fine')}>
              <span className={styles.choiceTitle}>
                Not a violation
                {suggested === 'not_violation' ? <span className={styles.suggested}>suggested</span> : null}
              </span>
              <span className={styles.choiceSub}>Dismiss the flag and mute re-flagging</span>
            </button>
            <button type="button" className={`${styles.choice} ${styles.choiceDanger}`} disabled={busy} onClick={() => setStep('violation')}>
              <span className={styles.choiceTitle}>
                Violation
                {suggested === 'violation' ? <span className={styles.suggested}>suggested · {timeline.summary.pattern}</span> : null}
              </span>
              <span className={styles.choiceSub}>End the broadcast, optionally ban</span>
            </button>
            {watch ? <WatchLink href={watch} /> : null}
            <p className={ui.dim}>Not sure yet? Leave it acknowledged: it resolves on its own if the signal clears.</p>
          </div>
        ) : null}

        {flag.state === 'acknowledged' && step === 'fine' ? (
          <form
            className={styles.stack}
            onSubmit={(e) => {
              e.preventDefault();
              void dismiss();
            }}
          >
            <fieldset className={styles.fieldset}>
              <legend>Why is it fine?</legend>
              <div className={styles.pills}>
                {DISMISS_REASONS.map((r) => (
                  <label key={r.value} className={reason === r.value ? styles.pillOn : styles.pill}>
                    <input type="radio" name="dismiss-reason" value={r.value} checked={reason === r.value} onChange={() => setReason(r.value)} />
                    {r.label}
                  </label>
                ))}
              </div>
            </fieldset>
            <fieldset className={styles.fieldset}>
              <legend>Stop re-flagging for</legend>
              {MUTE_SCOPES.map((s) => (
                <label key={s.value} className={scope === s.value ? styles.optionOn : styles.option}>
                  <input type="radio" name="dismiss-scope" value={s.value} checked={scope === s.value} onChange={() => setScope(s.value)} />
                  <span>
                    {s.label}
                    <span className={styles.optionHint}>{s.hint}</span>
                  </span>
                </label>
              ))}
            </fieldset>
            <div className={styles.formButtons}>
              <button type="button" disabled={busy} onClick={() => setStep('decide')}>
                Back
              </button>
              <button type="submit" className={ui.primary} disabled={busy}>
                Dismiss flag #{flag.id}
              </button>
            </div>
          </form>
        ) : null}

        {flag.state === 'acknowledged' && step === 'violation' ? (
          <form
            className={styles.stack}
            onSubmit={(e) => {
              e.preventDefault();
              void violation();
            }}
          >
            <p className={ui.sub}>
              The broadcast ends now{timeline.live ? '' : ' (it is not live, so this records the ban only)'}.
              {timeline.suggestion.why ? ` Suggested because ${timeline.suggestion.why}.` : ''}
            </p>
            <fieldset className={styles.fieldset}>
              <legend>And keep the broadcaster off for</legend>
              {BAN_CHOICES.map((c) => (
                <label key={c.id} className={ban === c.id ? styles.optionDangerOn : styles.option}>
                  <input type="radio" name="violation-ban" value={c.id} checked={ban === c.id} onChange={() => setBan(c.id)} />
                  <span>
                    {c.label}
                    <span className={styles.optionHint}>
                      {c.id === 'cooldown'
                        ? `A ${formatDuration(killCooldownSeconds * 1000)} cooldown so it cannot reconnect at once`
                        : c.id === 'permanent'
                          ? 'Until lifted under Bans'
                          : 'Banned by broadcast ID'}
                      {c.id === suggestedBan(timeline) && timeline.suggestion.verdict === 'violation' ? ' · suggested' : ''}
                    </span>
                  </span>
                </label>
              ))}
            </fieldset>
            {ban !== 'cooldown' && publisher ? <IpBan publisher={publisher} broadcast={broadcast} broadcasts={broadcasts} banIp={banIp} setBanIp={setBanIp} prefix={prefix} setPrefix={setPrefix} /> : null}
            <label className={styles.textField}>
              Reason (kept in the audit log)
              <input type="text" value={banReason} onChange={(e) => setBanReason(e.target.value)} />
            </label>
            <div className={styles.formButtons}>
              <button type="button" disabled={busy} onClick={() => setStep('decide')}>
                Back
              </button>
              <button type="submit" className={`${ui.danger} ${styles.confirmDanger}`} disabled={busy}>
                {ban === 'cooldown' ? 'End broadcast' : `End & ban ${BAN_CHOICES.find((c) => c.id === ban)?.label.toLowerCase()}`}
              </button>
            </div>
          </form>
        ) : null}
      </div>
    </aside>
  );
}

function IpBan({
  publisher,
  broadcast,
  broadcasts,
  banIp,
  setBanIp,
  prefix,
  setPrefix,
}: {
  publisher: NonNullable<ReturnType<typeof parseIp>>;
  broadcast: Broadcast | null;
  broadcasts: readonly Broadcast[];
  banIp: boolean;
  setBanIp: (v: boolean) => void;
  prefix: number;
  setPrefix: (n: number) => void;
}) {
  const shared = sharedIpVerdict(broadcasts, broadcast?.publisherRemoteIp);
  return (
    <div className={styles.stack}>
      {shared.warn ? (
        <p className={ui.warning} role="alert">
          <strong>
            {shared.sharing} of {shared.total} live broadcasts
          </strong>{' '}
          report this same publisher IP. The load balancer is probably not preserving client addresses, and an IP ban
          would block them all.
        </p>
      ) : null}
      <label className={styles.checkRow}>
        <input type="checkbox" checked={banIp} onChange={(e) => setBanIp(e.target.checked)} />
        Also ban the publisher IP <code className={ui.mono}>{publisher.ip}</code>
      </label>
      <label className={styles.checkRow}>
        Prefix
        <select value={String(prefix)} onChange={(e) => setPrefix(Number(e.target.value))} disabled={!banIp}>
          {prefixChoices(publisher.family).map((n) => (
            <option key={n} value={String(n)}>
              /{n}
            </option>
          ))}
        </select>
      </label>
      <p className={ui.caveat}>One address can be many people: an IP ban also blocks anyone sharing that NAT or network.</p>
    </div>
  );
}

function WatchLink({ href }: { href: string }) {
  return (
    <a className={styles.quietButton} href={href} target="_blank" rel="noreferrer">
      Watch live
    </a>
  );
}

function StateBadge({ flag }: { flag: ContentFlag }) {
  if (flag.mode === 'observe') return <span className={ui.badge}>observed</span>;
  if (flag.state === 'open') return <span className={ui.badgeBad}>open</span>;
  if (flag.state === 'acknowledged') return <span className={ui.badgeWarn}>acknowledged</span>;
  return <span className={ui.badge}>{describeResolution(flag)}</span>;
}
