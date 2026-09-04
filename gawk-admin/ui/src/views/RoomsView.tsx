import { useCallback, useEffect, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import { ApiError } from '../api/client.ts';
import type { Room, RoomWithSecret } from '../api/types.ts';
import { AuthRedirect } from '../auth/session.ts';
import { Dialog } from '../components/Dialog.tsx';
import { formatInstant } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import ui from '../styles/ui.module.css';

/**
 * Room management (R42, docs/44 D20): static rooms are created, gated with a
 * one-time attach secret and deleted here; dynamic rooms are listed and can be
 * ended, which deletes their CR and every relay pod's informer sees it.
 *
 * Raw room codes are on screen — the same three-places rule as broadcast IDs
 * (docs/42 D8, docs/44 D16): this OIDC-gated portal, the API, Postgres. They
 * never leave through a webhook, which carries the HMAC'd `key` instead, and
 * `?key=` on the route is how such a webhook lands on the right row.
 *
 * The attach secret is shown EXACTLY ONCE, in the reveal panel after a create
 * or a rotation. The API has no route that returns it again; an operator who
 * loses it rotates.
 */
export function RoomsView({ initialFilter = '' }: { initialFilter?: string }) {
  const api = useApi();
  const load = useCallback(() => api.rooms(), [api]);
  const { data, error, loading, reload } = useLoader<Room[]>(load);
  const rooms = data ?? [];

  const [filter, setFilter] = useState(initialFilter);
  // The route key re-asserts itself when it changes to a value, as on the
  // broadcasts view: a second webhook push while the tab is already here.
  useEffect(() => {
    if (initialFilter) setFilter(initialFilter);
  }, [initialFilter]);

  const [creating, setCreating] = useState(false);
  const [reveal, setReveal] = useState<RoomWithSecret | null>(null);
  const [rotating, setRotating] = useState<Room | null>(null);
  const [ending, setEnding] = useState<Room | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const needle = filter.trim().toLowerCase();
  const shown = needle
    ? rooms.filter(
        (r) =>
          r.name.toLowerCase().includes(needle) ||
          r.code.toLowerCase().includes(needle) ||
          (r.key ?? '').toLowerCase().includes(needle) ||
          (r.displayName ?? '').toLowerCase().includes(needle),
      )
    : rooms;

  // A 404 from the list is the deployment saying rooms are OFF (the route is
  // not registered without `-rooms`), not an empty list — and a deep link
  // from an old webhook should say so rather than show a blank table.
  const disabled = error !== null && /HTTP 404|no such endpoint/i.test(error);

  async function rotate(room: Room) {
    setBusy(true);
    setActionError(null);
    try {
      const result = await api.rotateRoomSecret(room.name);
      setRotating(null);
      setReveal(result);
      reload();
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function end(room: Room) {
    setBusy(true);
    setActionError(null);
    try {
      if (room.kind === 'dynamic') await api.endRoom(room.name);
      else await api.deleteRoom(room.name);
      setEnding(null);
      reload();
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <div className={ui.head}>
        <h1>Rooms</h1>
        <span className={ui.sub}>{rooms.length} in the cluster</span>
        <span className={ui.spacer} />
        <input
          aria-label="Filter rooms"
          placeholder="filter by code, name or key"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          disabled={disabled}
        />
        <button
          type="button"
          className={ui.primary}
          disabled={disabled}
          onClick={() => {
            setActionError(null);
            setCreating(true);
          }}
        >
          Create static room
        </button>
      </div>

      {disabled ? (
        <p className={ui.warning} role="status">
          Rooms are not enabled on this deployment: the chart’s <code>rooms.enabled</code> is
          off, so there is nothing to manage here (docs/44).
        </p>
      ) : error ? (
        <p className={ui.error}>{error}</p>
      ) : null}
      {actionError && !rotating && !ending && !creating ? (
        <p className={ui.error} role="alert">
          {actionError}
        </p>
      ) : null}

      {reveal ? <SecretReveal result={reveal} onDone={() => setReveal(null)} /> : null}

      {!disabled ? (
        <div className={ui.tableCard}>
          <table className={ui.table}>
            <thead>
              <tr>
                <th>Code</th>
                <th>Kind</th>
                <th>Name</th>
                <th>Broadcasts</th>
                <th>Home</th>
                <th>Key</th>
                <th>Gate</th>
                <th>Created</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((r) => (
                <tr key={r.name}>
                  <td className={ui.mono}>
                    <strong>{r.code}</strong>
                    {!r.managed && r.kind === 'static' ? (
                      <span
                        className={ui.badgeWarn}
                        title="Applied with kubectl, not created here; the portal deletes it only on your explicit request and leaves its Secret alone"
                      >
                        kubectl
                      </span>
                    ) : null}
                  </td>
                  <td>
                    <span className={r.kind === 'static' ? ui.badgeOk : ui.badge}>
                      {r.kind || 'unreadable'}
                    </span>
                  </td>
                  <td>{r.displayName ?? ''}</td>
                  <td>
                    {r.attachments}
                    {r.maxBroadcasts ? <span className={ui.dim}> / {r.maxBroadcasts}</span> : null}
                  </td>
                  <td className={ui.mono}>
                    {r.homeHolder ?? <span className={ui.dim}>not homed</span>}
                    {r.emptySince ? (
                      <span className={ui.dim} title={`empty since ${formatInstant(r.emptySince)}`}>
                        {' '}
                        (empty)
                      </span>
                    ) : null}
                  </td>
                  <td className={ui.mono}>{r.key ?? ''}</td>
                  <td>
                    {r.kind === 'dynamic' ? (
                      <span className={ui.dim}>creator token</span>
                    ) : r.hasAttachSecret ? (
                      <span className={ui.badgeOk}>attach secret</span>
                    ) : (
                      <span className={ui.badge}>open</span>
                    )}
                  </td>
                  <td className={ui.nowrap}>{r.createdAt ? formatInstant(r.createdAt) : ''}</td>
                  <td>
                    <div className={ui.actions}>
                      {r.kind === 'static' ? (
                        <button
                          type="button"
                          disabled={busy}
                          title={
                            r.hasAttachSecret
                              ? 'Mint a new attach secret; the old one stops working immediately'
                              : 'Add an attach secret to this open room'
                          }
                          onClick={() => {
                            setActionError(null);
                            setRotating(r);
                          }}
                        >
                          {r.hasAttachSecret ? 'Rotate secret' : 'Add secret'}
                        </button>
                      ) : null}
                      <button
                        type="button"
                        className={ui.danger}
                        disabled={busy}
                        onClick={() => {
                          setActionError(null);
                          setEnding(r);
                        }}
                      >
                        {r.kind === 'dynamic' ? 'End room' : 'Delete'}
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {!loading && !error && rooms.length === 0 ? (
        <p className={ui.dim}>
          No rooms. Static rooms are created here; dynamic ones appear when a broadcaster mints
          one.
        </p>
      ) : null}
      {!loading && !error && rooms.length > 0 && shown.length === 0 ? (
        <p className={ui.dim}>No room matches the filter.</p>
      ) : null}

      {creating ? (
        <CreateRoomForm
          onCancel={() => setCreating(false)}
          onCreated={(result) => {
            setCreating(false);
            setReveal(result);
            reload();
          }}
        />
      ) : null}

      {rotating ? (
        <Dialog
          title={`${rotating.hasAttachSecret ? 'Rotate' : 'Add'} attach secret for ${rotating.code}`}
          busy={busy}
          onCancel={() => setRotating(null)}
        >
          <p className={ui.sub}>
            {rotating.hasAttachSecret
              ? 'Every broadcaster holding the current secret is refused at its next attach. '
              : 'From now on attaching to this room needs the secret. '}
            The new secret is shown once, right after this — copy it somewhere before closing.
          </p>
          {actionError ? (
            <p className={ui.error} role="alert">
              {actionError}
            </p>
          ) : null}
          <div className={ui.actions}>
            <button type="button" className={ui.primary} disabled={busy} onClick={() => void rotate(rotating)}>
              {rotating.hasAttachSecret ? 'Rotate' : 'Add secret'}
            </button>
            <button type="button" disabled={busy} onClick={() => setRotating(null)}>
              Cancel
            </button>
          </div>
        </Dialog>
      ) : null}

      {ending ? (
        <Dialog
          title={ending.kind === 'dynamic' ? `End room ${ending.code}` : `Delete room ${ending.code}`}
          busy={busy}
          onCancel={() => setEnding(null)}
        >
          <p className={ui.sub}>
            {ending.kind === 'dynamic'
              ? 'Every participant is disconnected with “room ended” (close code 4007) and the code stops resolving. The attached broadcasts keep streaming to anyone watching them directly.'
              : 'The room and its portal-created attach secret are removed; the link stops working for everyone. Attached broadcasts are not touched.'}
          </p>
          {actionError ? (
            <p className={ui.error} role="alert">
              {actionError}
            </p>
          ) : null}
          <div className={ui.actions}>
            <button type="button" className={ui.danger} disabled={busy} onClick={() => void end(ending)}>
              {ending.kind === 'dynamic' ? 'End room' : 'Delete'}
            </button>
            <button type="button" disabled={busy} onClick={() => setEnding(null)}>
              Cancel
            </button>
          </div>
        </Dialog>
      ) : null}
    </section>
  );
}

/**
 * The one-time reveal. The value is rendered in a read-only input so it can
 * be selected and copied on a phone; there is deliberately no clipboard API
 * call — it is unavailable in insecure contexts and in tests, and a "Copy"
 * button that silently did nothing would lose the secret for good.
 */
function SecretReveal({ result, onDone }: { result: RoomWithSecret; onDone: () => void }) {
  if (!result.attachSecret) return null;
  return (
    <div className={ui.panel} role="status">
      <h2>Attach secret for {result.room.code}</h2>
      <p className={ui.warning}>
        Shown once. The portal cannot show it again — copy it now, or rotate later.
      </p>
      <div className={ui.field}>
        <label htmlFor="room-secret">Attach secret</label>
        <input
          id="room-secret"
          className={ui.mono}
          readOnly
          value={result.attachSecret}
          onFocus={(e) => e.currentTarget.select()}
        />
      </div>
      <p className={ui.dim}>
        Broadcasters pass it as <code className={ui.mono}>?attach=</code> on the room link, or in
        the native profile’s room field (docs/44 §4.8).
      </p>
      <div className={ui.actions}>
        <button type="button" onClick={onDone}>
          I have copied it
        </button>
      </div>
    </div>
  );
}

function CreateRoomForm({
  onCancel,
  onCreated,
}: {
  onCancel: () => void;
  onCreated: (result: RoomWithSecret) => void;
}) {
  const api = useApi();
  const [code, setCode] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [maxBroadcasts, setMaxBroadcasts] = useState('');
  const [withSecret, setWithSecret] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mirrors the server's refusal so the operator learns it before a round
  // trip; the server is still the authority (docs/44 D2, §4.2).
  const trimmed = code.trim();
  const looksDynamic = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$/i.test(trimmed);

  async function save() {
    setBusy(true);
    setError(null);
    try {
      const max = maxBroadcasts.trim() === '' ? 0 : Number(maxBroadcasts);
      const result = await api.createRoom({
        code: trimmed,
        ...(displayName.trim() ? { displayName: displayName.trim() } : {}),
        ...(max > 0 ? { maxBroadcasts: max } : {}),
        withAttachSecret: withSecret,
      });
      onCreated(result);
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      if (err instanceof ApiError && err.code === 'room_exists') {
        setError('A room with that code already exists.');
        return;
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={ui.panel}>
      <h2>Create static room</h2>
      <div className={ui.field}>
        <label htmlFor="room-code">Code (3–32 characters of A–Z, 0–9 and “-”; reached by link)</label>
        <input id="room-code" value={code} onChange={(e) => setCode(e.target.value)} />
        {looksDynamic ? (
          <p className={ui.warning} role="alert">
            Six characters of the broadcast alphabet look like a dynamic room code and would be
            typeable into the join box; static rooms are link-only (docs/44 D2). Add a character.
          </p>
        ) : null}
      </div>
      <div className={ui.field}>
        <label htmlFor="room-name">Display name (optional)</label>
        <input id="room-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
      </div>
      <div className={ui.field}>
        <label htmlFor="room-max">Max broadcasts (blank = fleet default)</label>
        <input
          id="room-max"
          type="number"
          min={1}
          max={64}
          value={maxBroadcasts}
          onChange={(e) => setMaxBroadcasts(e.target.value)}
        />
      </div>
      <label className={ui.row}>
        <input type="checkbox" checked={withSecret} onChange={(e) => setWithSecret(e.target.checked)} />
        Require an attach secret to stream into this room
      </label>
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}
      <div className={ui.actions}>
        <button
          type="button"
          className={ui.primary}
          disabled={busy || trimmed.length < 3 || looksDynamic}
          onClick={() => void save()}
        >
          Create
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
