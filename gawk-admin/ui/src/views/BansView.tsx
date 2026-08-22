import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import { enforcementNotice } from '../api/client.ts';
import type { Ban } from '../api/types.ts';
import { AuthRedirect } from '../auth/session.ts';
import { Dialog } from '../components/Dialog.tsx';
import { expiresIn, formatInstant } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

/**
 * The ban list (docs/42 §4.9): active by default, all of history on demand.
 *
 * Unban is a round trip, not an optimistic edit. `DELETE /api/v1/bans/{id}`
 * flips the row to `removed` AND deletes the Ban CR; if the CR delete fails the
 * ban is still enforced, so a row that vanished from this table on a click
 * would be a lie about what the fleet is doing. Reload and show what the server
 * says.
 *
 * That half-done case is a `202 Accepted` carrying the removed ban and its
 * `enforcement` sentence, and it is the one an operator is most likely to
 * misread — the row now says `removed` while the target is STILL banned. It
 * gets an amber notice of its own rather than the silence a clean `204` earns.
 */
export function BansView() {
  const api = useApi();
  const [stateFilter, setStateFilter] = useState<'active' | 'all'>('active');
  const load = useCallback(() => api.bans(stateFilter), [api, stateFilter]);
  const { data, loadedAt, error, loading, reload } = useLoader<Ban[]>(load);
  // A client-side filter matching the target, the source broadcast and the
  // actor: ban history grows monotonically (every kill adds a row), so the
  // scan-a-table-by-eye problem arrives here even sooner than on Broadcasts.
  const [filter, setFilter] = useState('');
  const [busyId, setBusyId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);
  // Unban is confirmed through the same modal the destructive actions use: it
  // lifts a ban fleet-wide within seconds, and for a target that is no longer
  // live the only way back from a mis-tap is kubectl — this portal's gating
  // manual pass is run from a phone (docs/42 §10).
  const [confirming, setConfirming] = useState<Ban | null>(null);

  const all = data ?? [];
  const needle = filter.trim().toLowerCase();
  const bans = needle
    ? all.filter(
        (b) =>
          b.target.value.toLowerCase().includes(needle) ||
          (b.sourceBroadcastId ?? '').toLowerCase().includes(needle) ||
          b.createdBy.toLowerCase().includes(needle),
      )
    : all;
  // Countdowns run from the fetch that produced these rows, not from render
  // time (see `useLoader`'s `loadedAt`).
  const now = loadedAt;

  async function unban(ban: Ban) {
    setBusyId(ban.id);
    setActionError(null);
    try {
      // null on a clean 204; the removed ban on a 202, whose `enforcement`
      // says the CR is still there and the target therefore still banned.
      const removed = await api.unban(ban.id);
      const detail = enforcementNotice(removed);
      setWarning(detail === null ? null : `${ban.target.value} — ${detail}`);
      setConfirming(null);
      reload();
    } catch (err) {
      // The session has started a full-page IdP redirect (session.ts): the
      // unban did not run, and the page is going away — nothing to render.
      if (err instanceof AuthRedirect) return;
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyId(null);
    }
  }

  return (
    <section>
      <div className={ui.head}>
        <h1>Bans</h1>
        <div className={ui.row}>
          <label className={ui.row}>
            <input
              type="radio"
              name="ban-filter"
              checked={stateFilter === 'active'}
              onChange={() => setStateFilter('active')}
            />
            Active
          </label>
          <label className={ui.row}>
            <input
              type="radio"
              name="ban-filter"
              checked={stateFilter === 'all'}
              onChange={() => setStateFilter('all')}
            />
            All
          </label>
        </div>
        <span className={ui.spacer} />
        <input
          type="search"
          aria-label="Filter bans"
          placeholder="Filter by target, broadcast or actor"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <button type="button" onClick={reload}>
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}
      {/* While the confirm dialog is open it renders the error itself. */}
      {actionError && !confirming ? (
        <p className={ui.error} role="alert">
          {actionError}
        </p>
      ) : null}
      {warning ? (
        <p className={ui.warning} role="alert">
          {warning}
        </p>
      ) : null}

      <div className={ui.scroll}>
        <table className={ui.table}>
          <thead>
            <tr>
              <th>Target</th>
              <th>State</th>
              <th>Expires</th>
              <th>Reason</th>
              <th>By</th>
              <th>Created</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {bans.map((b) => (
              <tr key={b.id} className={b.state === 'active' ? undefined : ui.recessed}>
                <td>
                  <span className={ui.badge}>{b.target.type}</span>{' '}
                  <code className={ui.mono}>{b.target.value}</code>
                </td>
                <td>
                  {b.state === 'active' ? (
                    <span className={ui.badgeBad}>active</span>
                  ) : (
                    <span className={ui.badge}>{b.state}</span>
                  )}
                </td>
                <td>{expiresIn(b.expiresAt, now)}</td>
                {/* Ban reasons are operator-private context (D8/§5): they render
                    here and live in Postgres, and relays log them at Debug only. */}
                <td>{b.reason || <span className={ui.dim}>—</span>}</td>
                <td>{b.createdBy}</td>
                <td className={ui.dim}>{formatInstant(b.createdAt)}</td>
                <td>
                  {b.state === 'active' ? (
                    <button
                      type="button"
                      disabled={busyId === b.id}
                      onClick={() => {
                        setActionError(null);
                        setConfirming(b);
                      }}
                    >
                      Unban
                    </button>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {!loading && bans.length === 0 && !error ? (
        needle ? (
          <p className={ui.dim}>
            Nothing matches “{filter.trim()}” ({all.length} {stateFilter === 'active' ? 'active ' : ''}
            bans).
          </p>
        ) : (
          <p className={ui.dim}>No {stateFilter === 'active' ? 'active ' : ''}bans.</p>
        )
      ) : null}

      {confirming ? (
        <Dialog
          title={`Unban ${confirming.target.value}`}
          busy={busyId !== null}
          onCancel={() => setConfirming(null)}
        >
          <p className={ui.sub}>
            Lifts the {confirming.target.type === 'ip' ? 'IP ban' : 'ban'} on{' '}
            <code className={ui.mono}>{confirming.target.value}</code> fleet-wide within seconds.
            Re-banning a target that is no longer live cannot be done from the portal.
          </p>
          {actionError ? (
            <p className={ui.error} role="alert">
              {actionError}
            </p>
          ) : null}
          <div className={ui.actions}>
            <button
              type="button"
              className={ui.danger}
              disabled={busyId !== null}
              onClick={() => void unban(confirming)}
            >
              Unban
            </button>
            <button type="button" disabled={busyId !== null} onClick={() => setConfirming(null)}>
              Cancel
            </button>
          </div>
        </Dialog>
      ) : null}
    </section>
  );
}
