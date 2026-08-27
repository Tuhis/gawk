import { useCallback, useEffect, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import { ApiError, enforcementNotice } from '../api/client.ts';
import type { ApiClient } from '../api/client.ts';
import type { Ban, Broadcast, BroadcastBanState, BroadcastsPage, CreateBanRequest } from '../api/types.ts';
import { AuthRedirect } from '../auth/session.ts';
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
  // The pre-filled filter from the route's `?key=` — how a webhook push's
  // portalUrl lands the operator on the offending row (§4.10).
  initialFilter = '',
  // The poll is a prop so it can be switched off (0). Production always uses
  // the default; a test that left a 5 s interval running would fire refetches
  // and out-of-act state updates in the middle of its own assertions.
  refreshMs = REFRESH_MS,
}: {
  killCooldownSeconds: number;
  initialFilter?: string;
  refreshMs?: number;
}) {
  const api = useApi();
  const load = useCallback(() => api.broadcasts(), [api]);
  const { data, loadedAt, error, loading, reload } = useLoader<BroadcastsPage>(load, refreshMs);

  // A client-side filter: at "hundreds of concurrent broadcasts" a paged
  // operator must not scan a fleet-sized table by eye mid-incident.
  const [filter, setFilter] = useState(initialFilter);
  // A WARM navigation must land like a cold one: with the tab already on
  // #/broadcasts, following a second push's ?key=… link changes only the
  // prop, so the route key re-asserts itself whenever it changes to a value.
  // An empty key (ordinary in-app navigation) never clobbers a typed filter.
  useEffect(() => {
    if (initialFilter) setFilter(initialFilter);
  }, [initialFilter]);

  const [killing, setKilling] = useState<Broadcast | null>(null);
  const [banning, setBanning] = useState<Broadcast | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  const all = data?.broadcasts ?? [];
  const needle = filter.trim().toLowerCase();
  const broadcasts = needle
    ? all.filter(
        (b) =>
          b.id.toLowerCase().includes(needle) ||
          b.key.toLowerCase().includes(needle) ||
          (b.publisherRemoteIp ?? '').toLowerCase().includes(needle),
      )
    : all;
  // Uptime is measured from the fetch that produced these rows, not from
  // whenever React re-rendered them (see `useLoader`'s `loadedAt`).
  const now = loadedAt;
  // Partial coverage must be VISIBLE: relayscan degrades an unreachable pod
  // into missing rows rather than an error, and this is the page an operator
  // trusts mid-incident — absence of knowledge must never read as knowledge
  // of absence (the rule BanCell's "unknown" already enforces).
  const partial = data !== null && data.podsAnswered < data.podsResolved;

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
      // "This page is going away", not an error to render (session.ts): the
      // kill did NOT run, and flashing red under a full-page IdP redirect
      // would be the last thing the operator half-reads.
      if (err instanceof AuthRedirect) return;
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
      // The IP ban goes FIRST, and the order is the whole point.
      //
      // The literal "publisher" is §4.7's contract: the server resolves the
      // live publisher's address through relayscan rather than trusting an
      // address this page read seconds ago. That resolve therefore has to run
      // before anything has started terminating the session — and the ID ban
      // is exactly that, twice over: it triggers the AP3 kill, and its handler
      // calls `afterMutation()`, which invalidates the relayscan fleet cache,
      // so an IP ban placed after it does a *fresh* scan racing that kill and
      // loses. Going first costs nothing, because the IP ban's own AP3
      // actuation ends the session just the same.
      const ip = draft.ip
        ? await attemptBan(api, {
            target: { type: 'ip', value: 'publisher', prefixLength: draft.ip.prefixLength },
            expiresAt: draft.expiresAt,
            reason: draft.reason,
            sourceBroadcastId: target.id,
          })
        : null;
      // And the ID ban is attempted WHATEVER happened to it. It is the action
      // that must land — the relay kills every live publisher matching a new
      // Ban (AP3), so it both stops the broadcast now and keeps it stopped —
      // and letting a 503 on the IP half abort it would leave the operator
      // with a live broadcast and nothing enforced at all.
      const id = await attemptBan(api, {
        target: { type: 'broadcastId', value: target.id },
        expiresAt: draft.expiresAt,
        reason: draft.reason,
        sourceBroadcastId: target.id,
      });

      // Either write can come back 202 on its own; one unenforced ban is
      // enough to make this amber, so the first notice wins.
      const pending = notice(target.id, banOf(id)) ?? notice(target.id, banOf(ip));
      const failure = partialFailure(target.id, id, ip);
      if (failure !== null) {
        // The dialog stays OPEN: both writes tolerate a 409, so clicking again
        // retries only what is missing — and once the broadcast has been
        // killed it leaves the table, taking that button with it.
        setNote(null);
        setWarning(pending);
        setActionError(failure);
        reload();
        return;
      }
      reportSuccess(
        pending,
        banSummary(target.id, id.ok && id.existed, ip === null ? null : ip.ok && ip.existed),
      );
      setBanning(null);
      reload();
    } catch (err) {
      // The one throw `attemptBan` lets through: the session has started a
      // full-page IdP redirect, so this page is going away (session.ts).
      if (err instanceof AuthRedirect) return;
      // Otherwise neither write throws — `attemptBan` answers with an outcome
      // — but a handler that can fail silently is worse than one line of belt.
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
          {needle ? `${broadcasts.length} of ${all.length} live` : `${all.length} live`}
          {refreshMs ? ` · refreshing every ${refreshMs / 1000}s` : ''}
        </span>
        <span className={ui.spacer} />
        <input
          type="search"
          aria-label="Filter broadcasts"
          placeholder="Filter by id, key or IP"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <button type="button" onClick={reload}>
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}
      {partial && data !== null ? (
        <p className={ui.warning} role="alert">
          {data.podsAnswered} of {data.podsResolved} relay pods answered; this list may be
          incomplete. The Relays view names the unreachable pods.
        </p>
      ) : null}
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
                  <BanCell state={b.banState} publisherActive={b.publisherActive} now={now} />
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

      {/* The reassuring empty state is earned only by FULL coverage: with pods
          unanswered, "nothing is broadcasting" is exactly the claim we cannot
          make, and the amber line above is already saying why. A filter that
          matched nothing is its own message — the fleet may be busy. */}
      {!loading && broadcasts.length === 0 && !error && !partial ? (
        needle ? (
          <p className={ui.dim}>
            Nothing matches “{filter.trim()}” ({all.length} live).
          </p>
        ) : (
          <p className={ui.dim}>Nothing is broadcasting right now.</p>
        )
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
          // The WHOLE fleet, not the filtered rows: the shared-IP verdict is a
          // statement about every live publisher, whatever the filter shows.
          broadcasts={all}
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

/**
 * The result of one `POST /bans`, as an outcome rather than an exception.
 *
 * Kill + ban runs two of these and the second must not be skipped because the
 * first failed, so neither may throw.
 */
type BanAttempt =
  | { ok: true; ban: Ban | null; existed: boolean }
  | { ok: false; error: string };

/**
 * Place a ban, treating `409 duplicate_active` as "already in force" and every
 * other failure as an outcome to report.
 *
 * A 409 is the one refusal that is not a refusal: it means the target already
 * carries an active ban — the state the operator is asking for — and it comes
 * back WITH that ban, so there is nothing to guess. That is what makes a retry
 * idempotent in either direction: whichever of the two writes already landed
 * is tolerated, and only the missing one is really re-sent.
 */
async function attemptBan(api: ApiClient, req: CreateBanRequest): Promise<BanAttempt> {
  try {
    return { ok: true, ban: await api.createBan(req), existed: false };
  } catch (err) {
    // Not an outcome: the session is mid-redirect to the IdP and nothing more
    // will run. Rendering it as a failed half would tell the operator a lie
    // on the way out of the page.
    if (err instanceof AuthRedirect) throw err;
    if (err instanceof ApiError && err.code === 'duplicate_active') {
      return { ok: true, ban: err.ban, existed: true };
    }
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

function banOf(attempt: BanAttempt | null): Ban | null {
  return attempt !== null && attempt.ok ? attempt.ban : null;
}

/**
 * The error line when kill + ban did not land in full, or null when it did.
 *
 * It has to name which half is in force, because that is what decides what the
 * operator does next — and, since `BansView` has no create form and a killed
 * broadcast leaves the table, this dialog is the only place they can do it.
 */
function partialFailure(
  id: string,
  idAttempt: BanAttempt,
  ipAttempt: BanAttempt | null,
): string | null {
  const idError = idAttempt.ok ? null : idAttempt.error;
  const ipError = ipAttempt === null || ipAttempt.ok ? null : ipAttempt.error;
  if (idError === null && ipError === null) return null;
  if (idError !== null && ipError !== null) {
    return `Nothing was banned. The broadcast: ${idError}. The publisher IP: ${ipError}.`;
  }
  if (idError !== null) {
    return ipAttempt === null
      ? idError
      : `The publisher IP is banned, but ${id} is not: ${idError}. Kill and ban again to retry it — the IP ban already in place will not be duplicated.`;
  }
  return `${id} is banned, but its publisher IP is not: ${ipError}. Kill and ban again to retry it — the broadcast ban already in place will not be duplicated.`;
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
 * How long a banned broadcast may keep visibly publishing before the cell says
 * the quiet part out loud. Three refresh cycles: enough for the relay's ban
 * informer and a kill to actuate and for the table to catch up, short enough
 * that a stuck projection is named while the operator is still looking.
 */
const ENFORCEMENT_LAG_MS = 15_000;

/**
 * Three states, not two — and the third has a divergent flavour.
 *
 * `banState: null` means AP4 could not reach Postgres and degraded this read
 * instead of failing it, so an operator can still see the fleet during a
 * database outage (§4.7). The broadcast rows are true; the ban column is simply
 * not known. Rendering that as "—" would assert the broadcast is clean, which
 * is the one claim we cannot make — and is exactly the confusion the null
 * exists to prevent.
 *
 * A banned row whose publisher is STILL live well after the ban was created is
 * the record-vs-enforcement divergence docs/42 worked hardest on — a Ban CR
 * that never landed, healing (or stuck) in the reconciler. The 202's amber
 * notice is ephemeral component state; this cell is the durable place the
 * contradiction the table is already displaying gets named, for whoever looks
 * later.
 */
function BanCell({
  state,
  publisherActive,
  now,
}: {
  state: BroadcastBanState | null;
  publisherActive: boolean;
  now: number;
}) {
  if (state === null) {
    return (
      <span className={ui.badgeWarn} title="the ban store was unreachable for this read">
        unknown
      </span>
    );
  }
  if (!state.banned) return <span className={ui.dim}>—</span>;
  const created = state.ban ? Date.parse(state.ban.createdAt) : Number.NaN;
  if (publisherActive && Number.isFinite(created) && now - created > ENFORCEMENT_LAG_MS) {
    return (
      <span
        className={ui.badgeWarn}
        title="This broadcast is banned but its publisher is still live — the enforcement object may not have landed. The Events and Relays views have the story."
      >
        banned — not enforced yet?
      </span>
    );
  }
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
