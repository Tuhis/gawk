import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { Ban } from '../api/types.ts';
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
 */
export function BansView() {
  const api = useApi();
  const [filter, setFilter] = useState<'active' | 'all'>('active');
  const load = useCallback(() => api.bans(filter), [api, filter]);
  const { data, error, loading, reload } = useLoader<Ban[]>(load);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const bans = data ?? [];
  const now = Date.now();

  async function unban(ban: Ban) {
    setBusyId(ban.id);
    setActionError(null);
    try {
      await api.unban(ban.id);
      reload();
    } catch (err) {
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
              checked={filter === 'active'}
              onChange={() => setFilter('active')}
            />
            Active
          </label>
          <label className={ui.row}>
            <input
              type="radio"
              name="ban-filter"
              checked={filter === 'all'}
              onChange={() => setFilter('all')}
            />
            All
          </label>
        </div>
        <span className={ui.spacer} />
        <button type="button" onClick={reload}>
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}
      {actionError ? (
        <p className={ui.error} role="alert">
          {actionError}
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
                      onClick={() => void unban(b)}
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
        <p className={ui.dim}>No {filter === 'active' ? 'active ' : ''}bans.</p>
      ) : null}
    </section>
  );
}
