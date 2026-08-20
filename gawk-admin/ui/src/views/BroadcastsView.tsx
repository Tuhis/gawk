import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { Broadcast } from '../api/types.ts';
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

  const broadcasts = data ?? [];
  // Uptime is measured from the fetch that produced these rows, not from
  // whenever React re-rendered them (see `useLoader`'s `loadedAt`).
  const now = loadedAt;

  async function confirmKill(draft: KillRequestDraft) {
    if (!killing) return;
    setBusy(true);
    setActionError(null);
    try {
      await api.kill(killing.id, {
        reason: draft.reason,
        cooldownSeconds: draft.cooldownSeconds,
      });
      setNote(`Killed ${killing.id}.`);
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
      await api.createBan({
        target: { type: 'broadcastId', value: target.id },
        expiresAt: draft.expiresAt,
        reason: draft.reason,
        sourceBroadcastId: target.id,
      });
      if (draft.ip) {
        // The literal "publisher" is §4.7's contract: the server resolves the
        // live publisher's address through relayscan rather than trusting an
        // address this page read seconds ago, and applies the prefix the
        // operator just confirmed.
        await api.createBan({
          target: { type: 'ip', value: 'publisher', prefixLength: draft.ip.prefixLength },
          expiresAt: draft.expiresAt,
          reason: draft.reason,
          sourceBroadcastId: target.id,
        });
      }
      setNote(`Banned ${target.id}${draft.ip ? ' and its publisher IP' : ''}.`);
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
                  {b.banState?.banned ? (
                    <span className={ui.badgeBad}>banned</span>
                  ) : (
                    <span className={ui.dim}>—</span>
                  )}
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
