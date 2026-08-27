import { useCallback } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { Relay } from '../api/types.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

/**
 * Per-pod effective relay configuration — **read-only, by decision** (docs/42
 * D10).
 *
 * There is deliberately no write path of any kind here: no form, no field, no
 * "apply". Relay configuration is the chart's, and a portal that could change
 * it would put the running fleet out of step with the manifest that is supposed
 * to describe it. This page exists to answer "what is that pod actually
 * running?" during an incident, and nothing more.
 *
 * The values come from `/internal/admin/config`, which the relay sanitizes at
 * the source (§4.5) — no secret reaches this page to be rendered.
 */
export function RelaysView() {
  const api = useApi();
  const load = useCallback(() => api.relays(), [api]);
  const { data, error, loading, reload } = useLoader<Relay[]>(load);
  const relays = data ?? [];

  return (
    <section>
      <div className={ui.head}>
        <h1>Relays</h1>
        <span className={ui.sub}>read-only — configuration belongs to the chart</span>
        <span className={ui.spacer} />
        <button type="button" onClick={reload}>
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}

      {relays.map((r) => (
        <article key={r.pod} className={ui.panel}>
          <div className={ui.row}>
            <strong className={ui.mono}>{r.pod}</strong>
            {r.reachable ? (
              <span className={ui.badgeOk}>reachable</span>
            ) : (
              <span className={ui.badgeBad}>unreachable</span>
            )}
            {r.version ? <span className={ui.dim}>{r.version}</span> : null}
          </div>
          {/* One pod failing to answer degrades that pod, never the aggregate
              (AP4) — so an unreachable pod still gets a row, with its error. */}
          {r.error ? <p className={ui.error}>{r.error}</p> : null}
          {r.config ? (
            <div className={ui.scroll}>
              <table className={ui.table}>
                <tbody>
                  {Object.entries(r.config)
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([k, v]) => (
                      <tr key={k}>
                        <th scope="row">{k}</th>
                        <td className={ui.mono}>{render(v)}</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </article>
      ))}

      {!loading && relays.length === 0 && !error ? (
        <p className={ui.dim}>No relay pods answered.</p>
      ) : null}
    </section>
  );
}

function render(v: unknown): string {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}
