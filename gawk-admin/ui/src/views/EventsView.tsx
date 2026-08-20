import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { EventPage, ModerationEvent, WebhookDelivery } from '../api/types.ts';
import { formatInstant } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

/**
 * The audit / notification feed (docs/42 §4.9), newest first, cursor-paginated
 * by `afterId`.
 *
 * Delivery state is rendered per event and NOT hidden behind a detail view:
 * "a failed delivery must be *seen*" (§4.10), because R40's posture — a flag
 * must reach a human — inherits this pipe. An event whose webhook never landed
 * looks different from one that did, at a glance, on this page.
 */
export function EventsView() {
  const api = useApi();
  // Pages accumulate. `nextAfterId` is always present on the response and is
  // non-null only when that page came back full, so null means the feed is
  // exhausted — the view stops offering "Load older" instead of paging into an
  // empty response.
  const [pages, setPages] = useState<ModerationEvent[]>([]);
  const [cursor, setCursor] = useState<number | undefined>(undefined);
  const [exhausted, setExhausted] = useState(false);

  const load = useCallback(async (): Promise<EventPage> => {
    const page = await api.events();
    setPages(page.events);
    setCursor(page.nextAfterId ?? undefined);
    setExhausted(page.nextAfterId === null);
    return page;
  }, [api]);
  const { error, loading, reload } = useLoader<EventPage>(load);

  const [olderError, setOlderError] = useState<string | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);

  async function loadOlder() {
    if (cursor === undefined) return;
    setLoadingOlder(true);
    setOlderError(null);
    try {
      const page = await api.events(cursor);
      setPages((prev) => [...prev, ...page.events]);
      setCursor(page.nextAfterId ?? undefined);
      setExhausted(page.nextAfterId === null);
    } catch (err) {
      setOlderError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingOlder(false);
    }
  }

  return (
    <section>
      <div className={ui.head}>
        <h1>Events</h1>
        <span className={ui.sub}>{pages.length} shown, newest first</span>
        <span className={ui.spacer} />
        <button
          type="button"
          onClick={() => {
            setExhausted(false);
            reload();
          }}
        >
          Refresh
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}

      {pages.map((e) => (
        <article key={e.id} className={ui.panel}>
          <div className={ui.row}>
            <span className={ui.badge}>{e.type}</span>
            <span className={ui.dim}>{formatInstant(e.occurredAt)}</span>
            <span className={ui.spacer} />
            <span className={ui.dim}>{e.actor}</span>
          </div>
          {e.broadcastId ? (
            <div>
              broadcast <code className={ui.mono}>{e.broadcastId}</code>
            </div>
          ) : null}
          {e.broadcastKey ? (
            <div className={ui.dim}>
              key <code className={ui.mono}>{e.broadcastKey}</code>
            </div>
          ) : null}
          {e.summary ? <p>{e.summary}</p> : null}
          {e.reason ? <p className={ui.dim}>reason: {e.reason}</p> : null}
          <Deliveries deliveries={e.deliveries} />
        </article>
      ))}

      {!loading && pages.length === 0 && !error ? (
        <p className={ui.dim}>No moderation events yet.</p>
      ) : null}

      {olderError ? <p className={ui.error}>{olderError}</p> : null}
      {pages.length > 0 && !exhausted ? (
        <button type="button" disabled={loadingOlder} onClick={() => void loadOlder()}>
          Load older
        </button>
      ) : null}
    </section>
  );
}

function Deliveries({ deliveries }: { deliveries?: WebhookDelivery[] }) {
  if (!deliveries || deliveries.length === 0) {
    // Zero configured webhooks is a supported state (§4.10) — events always
    // land here regardless — so say nothing rather than imply something broke.
    return null;
  }
  return (
    <div className={ui.row}>
      {deliveries.map((d) => (
        <span
          key={d.webhookName}
          className={
            d.state === 'failed' ? ui.badgeBad : d.state === 'delivered' ? ui.badgeOk : ui.badgeWarn
          }
          title={d.lastError ?? undefined}
        >
          {d.webhookName}: {d.state}
          {d.attempts > 1 ? ` (${d.attempts} attempts)` : ''}
          {d.state === 'failed' && d.lastError ? ` — ${d.lastError}` : ''}
        </span>
      ))}
    </div>
  );
}
