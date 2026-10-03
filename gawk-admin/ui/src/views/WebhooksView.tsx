import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import type { Webhook, WebhookEventName, WebhookTestResult } from '../api/types.ts';
import { AuthRedirect } from '@gawk/oidc-session';
import { Dialog } from '../components/Dialog.tsx';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

/**
 * The merged webhook list (docs/42 §4.10, D9).
 *
 * Webhooks come from two places and the difference is visible rather than
 * hidden. **Chart-defined** rows carry a lock and a `from config` badge: their
 * secrets live in Kubernetes Secrets and never enter the database, so the
 * portal cannot own them — any write addressing one is refused by the server
 * with `409 source_immutable`, and this page does not offer the button in the
 * first place. **UI-created** rows are full CRUD.
 *
 * Test-send works for BOTH: "visible, testable, never editable here" is the
 * whole point of showing a config row at all — an operator wiring up ntfy
 * needs to prove the pipe works without a redeploy.
 *
 * Secrets are shown exactly once: when the portal creates a webhook it
 * generates a Standard Webhooks `whsec_` secret (docs/52 D5) and displays it
 * in the create form for the operator to paste into the receiver — the one
 * moment the two sides can be given the same string. The API never returns a
 * secret (§4.7), so there is nothing to render afterwards; the edit form's
 * secret field is write-only and blank on every open.
 */
export function WebhooksView() {
  const api = useApi();
  const load = useCallback(() => api.webhooks(), [api]);
  const { data, error, loading, reload } = useLoader<Webhook[]>(load);
  const webhooks = data ?? [];

  const [editing, setEditing] = useState<Webhook | 'new' | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [tested, setTested] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);
  // Delete is confirmed through the modal: it silently removes a paging
  // channel — the pipe the "a flag must reach a human" posture rides on — and
  // re-creating one needs the signing secret, which this UI never shows.
  const [deleting, setDeleting] = useState<Webhook | null>(null);

  async function sendTest(w: Webhook) {
    setBusy(w.name);
    setActionError(null);
    try {
      const result: WebhookTestResult = await api.testWebhook(w.name);
      setTested((prev) => ({
        ...prev,
        [w.name]: result.ok
          ? `delivered${result.status ? ` (HTTP ${result.status})` : ''}`
          : `failed: ${result.error ?? `HTTP ${result.status ?? '?'}`}`,
      }));
    } catch (err) {
      // Mid-redirect to the IdP: the page is going away (session.ts).
      if (err instanceof AuthRedirect) return;
      setTested((prev) => ({
        ...prev,
        [w.name]: `failed: ${err instanceof Error ? err.message : String(err)}`,
      }));
    } finally {
      setBusy(null);
    }
  }

  async function remove(w: Webhook) {
    if (!w.id) return;
    setBusy(w.name);
    setActionError(null);
    try {
      await api.deleteWebhook(w.id);
      setDeleting(null);
      reload();
    } catch (err) {
      // Mid-redirect to the IdP: the delete did not run and the page is going
      // away (session.ts) — nothing to render.
      if (err instanceof AuthRedirect) return;
      // A `409 source_immutable` lands here if the server ever disagrees with
      // this page about a row's source. Surface its words rather than ours.
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <section>
      <div className={ui.head}>
        <h1>Webhooks</h1>
        <span className={ui.sub}>{webhooks.length} configured</span>
        <span className={ui.spacer} />
        <button type="button" className={ui.primary} onClick={() => setEditing('new')}>
          Add webhook
        </button>
      </div>

      {error ? <p className={ui.error}>{error}</p> : null}
      {/* While the delete confirm is open it renders the error itself. */}
      {actionError && !deleting ? (
        <p className={ui.error} role="alert">
          {actionError}
        </p>
      ) : null}

      <div className={ui.tableCard}>
        <table className={ui.table}>
          <thead>
            <tr>
              <th>Name</th>
              <th>URL</th>
              <th>Enabled</th>
              <th>Events</th>
              <th>Source</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {webhooks.map((w) => {
              const locked = w.source === 'config';
              return (
                <tr key={w.name}>
                  <td>
                    <div className={ui.row}>
                      {locked ? <span aria-hidden="true">🔒</span> : null}
                      <strong>{w.name}</strong>
                    </div>
                  </td>
                  <td className={`${ui.mono} ${ui.breakable}`}>{w.url}</td>
                  <td>
                    {w.enabled ? (
                      <span className={ui.badgeOk}>enabled</span>
                    ) : (
                      <span className={ui.badge}>disabled</span>
                    )}
                  </td>
                  <td>{describeEvents(w.events)}</td>
                  <td>
                    {locked ? (
                      <span className={ui.badgeWarn}>from config</span>
                    ) : (
                      <span className={ui.badge}>portal</span>
                    )}
                  </td>
                  <td>
                    <div className={ui.actions}>
                      {/* Test-send is offered for BOTH sources (§4.10). */}
                      <button
                        type="button"
                        disabled={busy === w.name}
                        onClick={() => void sendTest(w)}
                      >
                        Send test
                      </button>
                      {/* Disabled rather than absent: an operator who wonders why
                          they cannot edit this row deserves to be told, not to
                          hunt for a missing button. */}
                      <button
                        type="button"
                        disabled={locked}
                        title={
                          locked
                            ? 'Defined in the chart values; immutable in the portal (docs/42 D9)'
                            : undefined
                        }
                        onClick={() => setEditing(w)}
                      >
                        Edit
                      </button>
                      <button
                        type="button"
                        className={ui.danger}
                        disabled={locked || busy === w.name}
                        title={
                          locked
                            ? 'Defined in the chart values; immutable in the portal (docs/42 D9)'
                            : undefined
                        }
                        onClick={() => {
                          setActionError(null);
                          setDeleting(w);
                        }}
                      >
                        Delete
                      </button>
                      {tested[w.name] ? (
                        <span
                          className={
                            tested[w.name].startsWith('failed') ? ui.error : ui.ok
                          }
                          role="status"
                        >
                          {tested[w.name]}
                        </span>
                      ) : null}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      {!loading && webhooks.length === 0 && !error ? (
        <p className={ui.dim}>
          No webhooks. Events still land in the feed — a webhook only adds a push.
        </p>
      ) : null}

      {deleting ? (
        <Dialog
          title={`Delete webhook ${deleting.name}`}
          busy={busy !== null}
          onCancel={() => setDeleting(null)}
        >
          <p className={ui.sub}>
            Removes this delivery channel: events keep landing in the feed, but nothing pushes
            them to <code className={ui.mono}>{deleting.url}</code> any more. Re-creating it
            needs the signing secret, which the portal never shows.
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
              disabled={busy !== null}
              onClick={() => void remove(deleting)}
            >
              Delete
            </button>
            <button type="button" disabled={busy !== null} onClick={() => setDeleting(null)}>
              Cancel
            </button>
          </div>
        </Dialog>
      ) : null}

      {editing ? (
        <WebhookEditor
          webhook={editing === 'new' ? null : editing}
          onCancel={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reload();
          }}
          onError={setActionError}
        />
      ) : null}
    </section>
  );
}

const MODERATION_EVENTS: WebhookEventName[] = [
  'broadcast.killed',
  'ban.created',
  'ban.expired',
  'ban.removed',
  'content_flag.raised',
  'room.created',
  'room.ended',
  'room.secret_rotated',
];

const ACTIVITY_EVENTS: WebhookEventName[] = [
  'room.attached',
  'room.detached',
  'room.participant_joined',
  'room.participant_left',
];

const REJOINED: WebhookEventName = 'room.participant_rejoined';

/** The filter as a table cell: the default in words, a list as its size. */
function describeEvents(events: WebhookEventName[] | null | undefined) {
  if (events == null) return <span className={ui.dim}>moderation (default)</span>;
  if (events.length === 0) return <span className={ui.badgeWarn}>none</span>;
  return <span title={events.join(', ')}>{events.length} selected</span>;
}

/**
 * A fresh Standard Webhooks secret: `whsec_` + base64 of 24 random bytes,
 * the form every verifier library decodes itself (docs/52 D5). WebCrypto,
 * like pkce.ts — no dependency, and nothing the server has to be told.
 */
function generateSecret(): string {
  const buf = new Uint8Array(24);
  crypto.getRandomValues(buf);
  return 'whsec_' + btoa(String.fromCharCode(...buf));
}

function WebhookEditor({
  webhook,
  onCancel,
  onSaved,
  onError,
}: {
  webhook: Webhook | null;
  onCancel: () => void;
  onSaved: () => void;
  onError: (msg: string) => void;
}) {
  const api = useApi();
  const [name, setName] = useState(webhook?.name ?? '');
  const [url, setUrl] = useState(webhook?.url ?? '');
  // On CREATE the secret is generated here and shown, once, so the operator
  // can paste it into the receiver; on EDIT it is blank, because the API
  // never returns a secret and an editor that showed one would be the only
  // place in the system where a signing key is on screen after creation.
  const [secret, setSecret] = useState(() => (webhook ? '' : generateSecret()));
  const [enabled, setEnabled] = useState(webhook?.enabled ?? true);
  // The event filter (R49, docs/50 D8). "Default" sends null — every
  // moderation event, no room activity — which is what an ops pager wants;
  // a bot chooses the room events it posts about.
  const [custom, setCustom] = useState(webhook?.events != null);
  const [picked, setPicked] = useState<Set<WebhookEventName>>(
    () => new Set(webhook?.events ?? MODERATION_EVENTS),
  );
  const [busy, setBusy] = useState(false);

  function toggle(e: WebhookEventName, on: boolean) {
    setPicked((prev) => {
      const next = new Set(prev);
      if (on) next.add(e);
      else next.delete(e);
      return next;
    });
  }

  async function save() {
    setBusy(true);
    try {
      const body = {
        name: name.trim(),
        url: url.trim(),
        enabled,
        ...(secret ? { secret } : {}),
        // Always sent: an update REPLACES the filter, so leaving it out
        // would quietly reset a bot's subscription to the default.
        events: custom
          ? [...MODERATION_EVENTS, ...ACTIVITY_EVENTS, REJOINED].filter((e) => picked.has(e))
          : null,
      };
      if (webhook?.id) await api.updateWebhook(webhook.id, body);
      else await api.createWebhook(body);
      onSaved();
    } catch (err) {
      // Mid-redirect to the IdP: the save did not run and the page is going
      // away (session.ts) — nothing to render.
      if (err instanceof AuthRedirect) return;
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={ui.panel}>
      <h2>{webhook ? `Edit ${webhook.name}` : 'Add webhook'}</h2>
      <div className={ui.field}>
        <label htmlFor="wh-name">Name (unique across config and portal webhooks)</label>
        <input id="wh-name" value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <div className={ui.field}>
        <label htmlFor="wh-url">URL</label>
        <input id="wh-url" value={url} onChange={(e) => setUrl(e.target.value)} />
      </div>
      <div className={ui.field}>
        <label htmlFor="wh-secret">
          {webhook
            ? 'Signing secret (base64, leave blank to keep the current one)'
            : 'Signing secret (generated; copy it into the receiver now — it is never shown again)'}
        </label>
        <input
          id="wh-secret"
          type={webhook ? 'password' : 'text'}
          autoComplete="off"
          spellCheck={false}
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
        />
      </div>
      <label className={ui.row}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        Enabled
      </label>
      <fieldset className={ui.field}>
        <legend>Events</legend>
        <div className={ui.segmented}>
          <label className={ui.segment}>
            <input
              type="radio"
              name="wh-events"
              checked={!custom}
              onChange={() => setCustom(false)}
            />
            Moderation (default)
          </label>
          <label className={ui.segment}>
            <input
              type="radio"
              name="wh-events"
              checked={custom}
              onChange={() => setCustom(true)}
            />
            Choose events
          </label>
        </div>
        {custom ? (
          <>
            <p className={ui.dim}>Moderation</p>
            {MODERATION_EVENTS.map((e) => (
              <label key={e} className={ui.row}>
                <input type="checkbox" checked={picked.has(e)} onChange={(ev) => toggle(e, ev.target.checked)} />
                <code className={ui.mono}>{e}</code>
              </label>
            ))}
            <p className={ui.dim}>
              Room activity — needs the relay event bus. Carries room codes and nicknames.
            </p>
            {ACTIVITY_EVENTS.map((e) => (
              <label key={e} className={ui.row}>
                <input type="checkbox" checked={picked.has(e)} onChange={(ev) => toggle(e, ev.target.checked)} />
                <code className={ui.mono}>{e}</code>
              </label>
            ))}
            <label className={ui.row}>
              <input
                type="checkbox"
                checked={picked.has(REJOINED)}
                onChange={(ev) => toggle(REJOINED, ev.target.checked)}
              />
              <span>
                Also the reconnect pair when a room moves to another relay pod (
                <code className={ui.mono}>{REJOINED}</code>)
              </span>
            </label>
          </>
        ) : (
          <p className={ui.dim}>Every moderation event; no room activity.</p>
        )}
      </fieldset>
      <div className={ui.actions}>
        <button type="button" disabled={busy} onClick={() => void save()}>
          Save
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
