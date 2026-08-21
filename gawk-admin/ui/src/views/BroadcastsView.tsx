import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import { ApiError, enforcementNotice } from '../api/client.ts';
import type { ApiClient } from '../api/client.ts';
import type { Ban, Broadcast, BroadcastBanState, CreateBanRequest } from '../api/types.ts';
import { BanDialog } from '../components/BanDialog.tsx';
import type { BanRequestDraft } from '../components/BanDialog.tsx';
import { FlaggedPinSlot } from '../components/FlaggedPin.tsx';
import { KillDialog } from '../components/KillDialog.tsx';
import type { KillRequestDraft } from '../components/KillDialog.tsx';
import { uptime } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

const REFRESH_MS = 5_000;

/**
 * The fleet view, and the portal's landing page — `#/broadcasts` is also the
 * route every webhook payload's `portalUrl` points at (docs/42 §4.10), so it
 * has to be the thing a cold, deep-linked load resolves to.
 *
 * Raw broadcast IDs are on screen here, which is one of exactly three places
 * they are allowed to be (D8: the credential-gated relay admin API, this
 * OIDC-gated portal, and Postgres). They are joinable, so they never leave
 * through a webhook or a log.
 */
export function BroadcastsView({
  killCooldownSeconds,
  // The poll is a prop so it can be switched off (0). Production always uses
  // the default; a test that left a 5 s interval running would fire refetches
  // and out-of-act state updates in the middle of its own assertions.
  refreshMs = REFRESH_MS,
}: {
  killCooldownSeconds: number;
  refreshMs?: number;
}) {
  const api = useApi();
  const load = useCallback(() => api.broadcasts(), [api]);
  const { data, loadedAt, error, loading, reload } = useLoader<Broadcast[]>(load, refreshMs);

  const [killing, setKilling] = useState<Broadcast | null>(null);
  const [banning, setBanning] = useState<Broadcast | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  const broadcasts = data ?? [];
  // Uptime is measured from the fetch that produced these rows, not from
  // whenever React re-rendered them (see `useLoader`'s `loadedAt`).
  const now = loadedAt;

  /**
   * Report a mutation that succeeded, in one of its two flavours.
   *
   * `pending` is the server's `202` sentence: the ban row is committed and its
   * relay-side enforcement object is not written yet. That is neither the
   * green "done" nor the red "failed" — calling it either would mislead, so it
   * gets the amber notice and the success line is suppressed.
   */
  function reportSuccess(pending: string | null, done: string) {
    setNote(pending ? null : done);
    setWarning(pending);
  }

  async function confirmKill(draft: KillRequestDraft) {
    if (!killing) return;
    const target = killing;
    setBusy(true);
    setActionError(null);
    try {
      const { ban } = await api.kill(target.id, {
        reason: draft.reason,
        cooldownSeconds: draft.cooldownSeconds,
      });
      reportSuccess(notice(target.id, ban), `Killed ${target.id}.`);
      setKilling(null);
      reload();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function confirmBan(draft: BanRequestDraft) {
    if (!banning) return;
    const target = banning;
    setBusy(true);
    setActionError(null);
    try {
      // The ID ban is the action that ends the broadcast: the relay kills every
      // live publisher matching a new Ban (AP3), so this both stops it now and
      // keeps it stopped.
      const idBan = await ensureBan(api, {
        target: { type: 'broadcastId', value: target.id },
        expiresAt: draft.expiresAt,
        reason: draft.reason,
        sourceBroadcastId: target.id,
      });
      // Either write can come back 202 on its own; one unenforced ban is
      // enough to make this amber, so the first notice wins.
      let pending = notice(target.id, idBan.ban);
      let ipBan: BanOutcome | null = null;
      if (draft.ip) {
        // The literal "publisher" is §4.7's contract: the server resolves the
        // live publisher's address through relayscan rather than trusting an
        // address this page read seconds ago, and applies the prefix the
        // operator just confirmed.
        ipBan = await ensureBan(api, {
          target: { type: 'ip', value: 'publisher', prefixLength: draft.ip.prefixLength },
          expiresAt: draft.expiresAt,
          reason: draft.reason,
          sourceBroadcastId: target.id,
        });
        pending = pending ?? notice(target.id, ipBan.ban);
      }
      reportSuccess(
        pending,
        banSummary(target.id, idBan.existed, ipBan === null ? null : ipBan.existed),
      );
      setBanning(null);
      reload();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <div className={ui.head}>
        <h1>Broadcasts</h1>
        <span className={ui.sub}>
          {broadcasts.length} live
          {refreshMs ? ` · refreshing every ${refreshMs / 1000}s` : ''}
        </span>
        <span className={ui.spacer} />
        <button type="button" onClick={reload}>
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}
      {note ? <p className={ui.ok}>{note}</p> : null}
      {warning ? (
        <p className={ui.warning} role="alert">
          {warning}
        </p>
      ) : null}

      <div className={ui.scroll}>
        <table className={ui.table}>
          <thead>
            <tr>
              <th>Broadcast</th>
              <th>Publisher</th>
              <th>Uptime</th>
              <th>Viewers</th>
              <th>Pods</th>
              <th>Ban</th>
              <th>Links</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {broadcasts.map((b) => (
              <tr key={b.id} className={b.publisherActive ? undefined : ui.recessed}>
                <td>
                  <div className={ui.row}>
                    <FlaggedPinSlot broadcast={b} />
                    <code className={ui.mono}>{b.id}</code>
                    <CopyButton value={b.id} />
                  </div>
                  <div className={ui.dim}>
                    key <code className={ui.mono}>{b.key}</code>
                  </div>
                </td>
                <td>
                  {b.publisherActive ? (
                    <span className={ui.badgeOk}>live</span>
                  ) : (
                    <span className={ui.badge}>away</span>
                  )}
                  <div className={ui.mono}>{b.publisherRemoteIp ?? '—'}</div>
                </td>
                <td>{uptime(b.startedAt, now)}</td>
                <td>{b.viewersGlobal}</td>
                <td>
                  {b.pods.length === 0 ? (
                    <span className={ui.dim}>—</span>
                  ) : (
                    b.pods.map((p) => (
                      <div key={p.pod} className={ui.dim}>
                        <span className={ui.mono}>{p.pod}</span> {p.role} · {p.viewersLocal}
                      </div>
                    ))
                  )}
                </td>
                <td>
                  <BanCell state={b.banState} />
                </td>
                <td>
                  <div className={ui.actions}>
                    {/* Omitted by the server when -app-base-url / -telemetry-base-url
                        are unset (§4.12). A link to nowhere is worse than none. */}
                    {b.links?.watch ? (
                      <a href={b.links.watch} target="_blank" rel="noreferrer">
                        Watch
                      </a>
                    ) : null}
                    {b.links?.telemetry ? (
                      <a href={b.links.telemetry} target="_blank" rel="noreferrer">
                        Telemetry
                      </a>
                    ) : null}
                  </div>
                </td>
                <td>
                  <div className={ui.actions}>
                    <button type="button" onClick={() => setKilling(b)}>
                      Kill
                    </button>
                    <button type="button" className={ui.danger} onClick={() => setBanning(b)}>
                      Kill + ban
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {!loading && broadcasts.length === 0 && !error ? (
        <p className={ui.dim}>Nothing is broadcasting right now.</p>
      ) : null}

      {killing ? (
        <KillDialog
          broadcast={killing}
          defaultCooldownSeconds={killCooldownSeconds}
          busy={busy}
          error={actionError}
          onCancel={() => {
            setKilling(null);
            setActionError(null);
          }}
          onConfirm={(d) => void confirmKill(d)}
        />
      ) : null}

      {banning ? (
        <BanDialog
          broadcast={banning}
          broadcasts={broadcasts}
          busy={busy}
          error={actionError}
          onCancel={() => {
            setBanning(null);
            setActionError(null);
          }}
          onConfirm={(d) => void confirmBan(d)}
        />
      ) : null}
    </section>
  );
}

/** What one `POST /bans` left behind: the ban, and whether it predates the click. */
interface BanOutcome {
  /** The created ban, or the existing one a `409 duplicate_active` returned. */
  ban: Ban | null;
  existed: boolean;
}

/**
 * Place a ban, treating `409 duplicate_active` as "already in force".
 *
 * Kill + ban is two sequential writes and the second is the fragile one: the
 * first ban's `afterMutation()` invalidates the relayscan fleet cache, so the
 * IP ban's server-side `"publisher"` resolve does a *fresh* scan racing the
 * kill the first ban just triggered — if the publisher has already gone it
 * 400s. Without this, the retry that failure demands re-sent the ID ban, got a
 * 409, and aborted before ever reaching the IP ban: `BansView` has no create
 * form and the killed broadcast has left the table, so the IP ban became
 * unreachable from the UI entirely.
 *
 * A 409 is the one refusal that is not a refusal. It means the target already
 * carries an active ban — the state the operator is asking for — and it comes
 * back WITH that ban, so there is nothing to guess. Every other failure still
 * throws.
 */
async function ensureBan(api: ApiClient, req: CreateBanRequest): Promise<BanOutcome> {
  try {
    return { ban: await api.createBan(req), existed: false };
  } catch (err) {
    if (err instanceof ApiError && err.code === 'duplicate_active') {
      return { ban: err.ban, existed: true };
    }
    throw err;
  }
}

/**
 * The confirmation line for a kill + ban.
 *
 * It has to stay true when a target was already banned. "Banned ABC123." would
 * claim this click did something it did not — in a portal whose whole job is
 * the moderation record — and a red error would claim nothing happened when
 * the state the operator asked for is the state that exists.
 */
function banSummary(id: string, idExisted: boolean, ipExisted: boolean | null): string {
  if (ipExisted === null) return idExisted ? `${id} was already banned.` : `Banned ${id}.`;
  if (!idExisted && !ipExisted) return `Banned ${id} and its publisher IP.`;
  if (idExisted && ipExisted) return `${id} and its publisher IP were already banned.`;
  if (idExisted) return `Banned ${id}'s publisher IP; ${id} was already banned.`;
  return `Banned ${id}; its publisher IP was already banned.`;
}

/**
 * A ban that was RECORDED but is not yet ENFORCED — the server's `202
 * Accepted`, not an error.
 *
 * The Postgres row is committed; only the projection to a Ban CR failed, and
 * the reconciler will heal it. So this is neither a plain success nor a
 * failure, and calling it either would mislead: "failed" invites a retry that
 * will now 409 against the row that does exist, and "done" claims an
 * enforcement that has not started. The sentence is the server's — it is the
 * side that knows which way the two are out of step — prefixed with the
 * broadcast so the operator knows which row it is about.
 */
function notice(id: string, ban: Ban | null | undefined): string | null {
  const detail = enforcementNotice(ban);
  return detail === null ? null : `${id} — ${detail} The broadcast stays live until it succeeds.`;
}

/**
 * Three states, not two.
 *
 * `banState: null` means AP4 could not reach Postgres and degraded this read
 * instead of failing it, so an operator can still see the fleet during a
 * database outage (§4.7). The broadcast rows are true; the ban column is simply
 * not known. Rendering that as "—" would assert the broadcast is clean, which
 * is the one claim we cannot make — and is exactly the confusion the null
 * exists to prevent.
 */
function BanCell({ state }: { state: BroadcastBanState | null }) {
  if (state === null) {
    return (
      <span className={ui.badgeWarn} title="the ban store was unreachable for this read">
        unknown
      </span>
    );
  }
  if (!state.banned) return <span className={ui.dim}>—</span>;
  return <span className={ui.badgeBad}>banned</span>;
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      title="Copy the broadcast ID"
      onClick={() => {
        // Absent on an insecure origin and in some embedded browsers; the ID is
        // selectable text either way, so failure is silent rather than loud.
        void navigator.clipboard?.writeText(value).then(
          () => setCopied(true),
          () => undefined,
        );
      }}
    >
      {copied ? 'copied' : 'copy'}
    </button>
  );
}
