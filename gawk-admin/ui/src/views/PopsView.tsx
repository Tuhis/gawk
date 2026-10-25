import { useCallback, useState } from 'react';

import { useApi } from '../auth/AuthContext.tsx';
import { ApiError } from '../api/client.ts';
import type { CreatePopRequest, Pop, PopBudgetRequest, PopPatchRequest, PopWithPairing } from '../api/types.ts';
import { AuthRedirect } from '@gawk/oidc-session';
import { Dialog } from '../components/Dialog.tsx';
import { formatInstant, uptime } from '../lib/format.ts';
import { useLoader } from '../lib/useLoader.ts';
import { href, popHref } from '../router/router.ts';
import ui from '../styles/ui.module.css';

/**
 * PoP relays (R74, docs/76 §7.2): the cards, the four-step New PoP wizard and
 * a PoP's detail page (`#/pops/<name>`).
 *
 * The pairing string is shown EXACTLY ONCE, after a create or a re-pair. The
 * API stores only its SHA-256 and has no route that returns it again; an
 * operator who loses it re-pairs.
 *
 * Addresses are on screen — the PoP's public and LAN addresses and its
 * publishers' LAN addresses — because this portal may show them (docs/76 §8);
 * no event ever carries one.
 *
 * Copy is provisional until the PP1 design pass.
 */
export function PopsView({
  detail = '',
  namedAddress = false,
  rooms = false,
}: {
  /** `#/pops/<name>`: one PoP's detail page. */
  detail?: string;
  /** `me.features.popNamedAddress`: the named tier is configured. */
  namedAddress?: boolean;
  /** `me.features.rooms`: a party wall is a room, so it needs rooms. */
  rooms?: boolean;
}) {
  if (detail) return <PopDetail name={detail} namedAddress={namedAddress} />;
  return <PopList namedAddress={namedAddress} rooms={rooms} />;
}

const STATE_LABEL: Record<Pop['state'], string> = {
  'waiting-for-pairing': 'Waiting for pairing',
  linked: 'Linked',
  offline: 'Offline',
  revoked: 'Revoked',
  ended: 'Ended',
  invalid: 'Unreadable',
};

const STATE_BADGE: Record<Pop['state'], string> = {
  'waiting-for-pairing': ui.badgeWarn,
  linked: ui.badgeOk,
  offline: ui.badgeWarn,
  revoked: ui.badgeBad,
  ended: ui.badge,
  invalid: ui.badgeBad,
};

/** "Linked 3h 04m", "Offline since …", or the bare state. */
function stateText(p: Pop, now: number): string {
  if (p.state === 'linked' && p.stateSince) return `Linked ${uptime(p.stateSince, now)}`;
  if (p.state === 'offline' && p.stateSince) return `Offline since ${formatInstant(p.stateSince)}`;
  return STATE_LABEL[p.state] ?? p.state;
}

function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'pop_exists':
        return 'A PoP with that name already exists.';
      case 'wall_code_taken':
        return 'The wall code is already a room; choose another.';
      case 'pop_revoked':
        return 'The PoP is revoked: a new pairing string would pair nothing.';
    }
  }
  return err instanceof Error ? err.message : String(err);
}

function PopList({ namedAddress, rooms }: { namedAddress: boolean; rooms: boolean }) {
  const api = useApi();
  const load = useCallback(() => api.pops(), [api]);
  const { data, error, loading, loadedAt, reload } = useLoader<Pop[]>(load, 15_000);
  const pops = data ?? [];
  const [creating, setCreating] = useState(false);
  const [reveal, setReveal] = useState<PopWithPairing | null>(null);

  // A 404 is the deployment saying PoPs are OFF (the routes are not
  // registered without -pops), not an empty list.
  const disabled = error !== null && /HTTP 404|no such endpoint/i.test(error);

  return (
    <section>
      <div className={ui.head}>
        <h1>PoPs</h1>
        <span className={ui.sub}>{pops.length} in the cluster</span>
        <span className={ui.spacer} />
        <button
          type="button"
          className={ui.primary}
          disabled={disabled || creating}
          onClick={() => {
            setReveal(null);
            setCreating(true);
          }}
        >
          New PoP
        </button>
      </div>

      {disabled ? (
        <p className={ui.warning} role="status">
          PoPs are not enabled on this deployment: the chart’s <code>pops.enabled</code> is off
          (docs/76).
        </p>
      ) : error ? (
        <p className={ui.error}>{error}</p>
      ) : null}

      {reveal ? <PairingReveal result={reveal} onDone={() => setReveal(null)} /> : null}

      {creating ? (
        <NewPopWizard
          namedAddress={namedAddress}
          rooms={rooms}
          onCancel={() => setCreating(false)}
          onCreated={(result) => {
            setCreating(false);
            setReveal(result);
            reload();
          }}
        />
      ) : null}

      {!disabled ? (
        <div className={ui.grid2}>
          {pops.map((p) => (
            <PopCard key={p.name} pop={p} now={loadedAt} />
          ))}
        </div>
      ) : null}
      {!loading && !error && pops.length === 0 ? (
        <p className={ui.dim}>No PoPs. Create one for a LAN party, then paste its pairing string into the relay.</p>
      ) : null}
    </section>
  );
}

function PopCard({ pop: p, now }: { pop: Pop; now: number }) {
  const live = p.live;
  return (
    <article className={ui.panel}>
      <h2>
        <a href={popHref(p.name)}>{p.displayName || p.name}</a>
      </h2>
      <p className={ui.mono}>{p.name}</p>
      <p>
        <span className={STATE_BADGE[p.state] ?? ui.badge}>{stateText(p, now)}</span>
      </p>
      <p className={ui.dim}>
        {[
          p.link?.version ? `version ${p.link.version}` : '',
          p.createdAt ? `created ${formatInstant(p.createdAt)}` : '',
          p.notAfter ? `ends ${formatInstant(p.notAfter)}` : '',
        ]
          .filter(Boolean)
          .join(' · ')}
      </p>
      {live ? (
        <>
          <p>{`${live.live} live · ${live.forwarded} forwarded`}</p>
          <p className={ui.dim}>{`${live.outsideViewers} outside viewers`}</p>
          <UplinkGauge pop={p} />
          <p className={ui.dim}>{`${live.offersSent} offers · ${live.guestsConnected} guests here`}</p>
        </>
      ) : null}
    </article>
  );
}

function UplinkGauge({ pop: p }: { pop: Pop }) {
  if (!p.live) return null;
  const ceiling = p.live.uplink.maxMbps || p.uplink.maxMbps;
  const streams = p.live.uplink.maxStreams || p.uplink.maxStreams;
  const mbps = Math.round(p.live.uplinkMbps * 10) / 10;
  return (
    <div className={ui.field}>
      <progress max={ceiling || 1} value={Math.min(mbps, ceiling || 1)} aria-label="Uplink use" />
      <span>{`Uplink ${mbps} / ${ceiling} Mbps · ${p.live.uplinkStreams} / ${streams} streams`}</span>
    </div>
  );
}

const NAME_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const DYNAMIC_RE = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$/i;
const ROOM_CODE_RE = /^[A-Za-z0-9]([-A-Za-z0-9]{1,30}[A-Za-z0-9])$/;

/** A blank field is "the default" and is not sent. */
function num(v: string): number | undefined {
  const t = v.trim();
  if (t === '') return undefined;
  const n = Number(t);
  return Number.isFinite(n) ? n : undefined;
}

function budget(mbps: string, streams: string): PopBudgetRequest | undefined {
  const b: PopBudgetRequest = {};
  const m = num(mbps);
  const s = num(streams);
  if (m !== undefined) b.maxMbps = m;
  if (s !== undefined) b.maxStreams = s;
  return Object.keys(b).length ? b : undefined;
}

interface BudgetDraft {
  upMbps: string;
  upStreams: string;
  downMbps: string;
  downStreams: string;
  maxBroadcasts: string;
}

function BudgetFields({ draft, set }: { draft: BudgetDraft; set: (d: BudgetDraft) => void }) {
  const field = (id: string, label: string, key: keyof BudgetDraft, max: number) => (
    <div className={ui.field}>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        type="number"
        min={0}
        max={max}
        value={draft[key]}
        onChange={(e) => set({ ...draft, [key]: e.target.value })}
      />
    </div>
  );
  return (
    <>
      {field('pop-up-mbps', 'Uplink Mbps (blank = 25)', 'upMbps', 10000)}
      {field('pop-up-streams', 'Uplink streams (blank = 3)', 'upStreams', 64)}
      {field('pop-down-mbps', 'Downlink Mbps (blank = unlimited)', 'downMbps', 10000)}
      {field('pop-down-streams', 'Downlink streams (blank = unlimited)', 'downStreams', 64)}
      {field('pop-max-broadcasts', 'Party broadcasts home carries (blank = 40)', 'maxBroadcasts', 1000)}
    </>
  );
}

const EMPTY_BUDGET: BudgetDraft = { upMbps: '', upStreams: '', downMbps: '', downStreams: '', maxBroadcasts: '' };

function NewPopWizard({
  namedAddress,
  rooms,
  onCancel,
  onCreated,
}: {
  namedAddress: boolean;
  rooms: boolean;
  onCancel: () => void;
  onCreated: (result: PopWithPairing) => void;
}) {
  const api = useApi();
  const [step, setStep] = useState(1);
  const [name, setName] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [notAfter, setNotAfter] = useState('');
  const [draft, setDraft] = useState<BudgetDraft>(EMPTY_BUDGET);
  const [autoSwitch, setAutoSwitch] = useState(true);
  const [wall, setWall] = useState(false);
  const [wallCode, setWallCode] = useState('');
  const [named, setNamed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const trimmed = name.trim();
  const nameOk = NAME_RE.test(trimmed);
  // Mirrors pops.ValidateWallCode so a long or code-shaped name is caught
  // before a round trip; the server is still the authority.
  const code = wallCode.trim() || trimmed;
  const wallOk = !wall || (ROOM_CODE_RE.test(code) && !DYNAMIC_RE.test(code));

  async function create() {
    setBusy(true);
    setError(null);
    try {
      const req: CreatePopRequest = { name: trimmed };
      if (displayName.trim()) req.displayName = displayName.trim();
      if (notAfter) req.notAfter = new Date(notAfter).toISOString();
      const up = budget(draft.upMbps, draft.upStreams);
      const down = budget(draft.downMbps, draft.downStreams);
      if (up) req.uplink = up;
      if (down) req.downlink = down;
      const max = num(draft.maxBroadcasts);
      if (max !== undefined) req.limits = { maxBroadcasts: max };
      req.guests = { autoSwitch };
      if (wall) req.wall = wallCode.trim() ? { roomCode: wallCode.trim() } : {};
      if (named) req.namedAddress = true;
      onCreated(await api.createPop(req));
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={ui.panel}>
      <h2>{`New PoP · step ${step} of 4`}</h2>
      {step === 1 ? (
        <>
          <div className={ui.field}>
            <label htmlFor="pop-name">Name (a-z, 0-9 and “-”, at most 63; also the default wall code)</label>
            <input id="pop-name" value={name} onChange={(e) => setName(e.target.value)} />
            {trimmed && !nameOk ? (
              <p className={ui.warning} role="alert">
                Lower-case letters, digits and “-”, not starting or ending with “-”.
              </p>
            ) : null}
          </div>
          <div className={ui.field}>
            <label htmlFor="pop-display">Display name (optional)</label>
            <input id="pop-display" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
          </div>
          <div className={ui.field}>
            <label htmlFor="pop-end">End date (optional; links are refused after it)</label>
            <input id="pop-end" type="datetime-local" value={notAfter} onChange={(e) => setNotAfter(e.target.value)} />
          </div>
        </>
      ) : null}
      {step === 2 ? (
        <>
          <p className={ui.sub}>Party internet: what the venue’s connection can give home.</p>
          <BudgetFields draft={draft} set={setDraft} />
        </>
      ) : null}
      {step === 3 ? (
        <>
          <label className={ui.row}>
            <input type="checkbox" checked={autoSwitch} onChange={(e) => setAutoSwitch(e.target.checked)} />
            Auto-switch: home suggests this relay to guests on the party network
          </label>
          <label className={ui.row}>
            <input type="checkbox" checked={wall} disabled={!rooms} onChange={(e) => setWall(e.target.checked)} />
            Party wall: every party broadcast on one room page
          </label>
          {!rooms ? <p className={ui.dim}>A party wall is a room: it needs rooms enabled on this deployment.</p> : null}
          {wall ? (
            <div className={ui.field}>
              <label htmlFor="pop-wall-code">Wall code (blank = the name)</label>
              <input id="pop-wall-code" value={wallCode} onChange={(e) => setWallCode(e.target.value)} />
              {!wallOk ? (
                <p className={ui.warning} role="alert">
                  A wall code is 3–32 characters of A–Z, 0–9 and “-” and must not look like a six-character
                  broadcast code; set one explicitly.
                </p>
              ) : null}
            </div>
          ) : null}
          <label className={ui.row}>
            <input type="checkbox" checked={named} disabled={!namedAddress} onChange={(e) => setNamed(e.target.checked)} />
            Named address: a host name and certificate, so phones load the relay with no prompt
          </label>
          {!namedAddress ? <p className={ui.dim}>Not configured: needs -pop-dns-zone and -pop-cert-issuer.</p> : null}
        </>
      ) : null}
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}
      <div className={ui.actions}>
        {step > 1 ? (
          <button type="button" disabled={busy} onClick={() => setStep(step - 1)}>
            Back
          </button>
        ) : null}
        {step < 3 ? (
          <button type="button" className={ui.primary} disabled={!nameOk} onClick={() => setStep(step + 1)}>
            Next
          </button>
        ) : (
          <button type="button" className={ui.primary} disabled={busy || !nameOk || !wallOk} onClick={() => void create()}>
            Create and pair
          </button>
        )}
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

/**
 * Step 4, and the re-pair answer: the pairing string once. A read-only input
 * so it can be selected on a phone; no clipboard call, for RoomsView's reason
 * (a Copy button that silently failed would lose the string).
 */
function PairingReveal({ result, onDone }: { result: PopWithPairing; onDone: () => void }) {
  const docker = `docker run --network host -v gawk-relay:/data -e GAWK_PAIR='${result.pairingString}' ghcr.io/tuhis/gawk-relay`;
  return (
    <div className={ui.panel} role="status">
      <h2>Pair {result.pop.displayName || result.pop.name}</h2>
      <p className={ui.warning}>
        Shown once. The portal stores only its hash and cannot show it again — copy it now, or re-pair later.
      </p>
      <div className={ui.field}>
        <label htmlFor="pop-pairing">Pairing string</label>
        <input
          id="pop-pairing"
          className={ui.mono}
          readOnly
          value={result.pairingString}
          onFocus={(e) => e.currentTarget.select()}
        />
      </div>
      <p className={ui.dim}>{`Redeemable once, until ${formatInstant(result.expiresAt)}.`}</p>
      <p>
        Paste it into the relay’s page at <code className={ui.mono}>http://127.0.0.1:4480</code>, or start the
        container with it:
      </p>
      <p>
        <code className={ui.mono}>{docker}</code>
      </p>
      <p>
        <a href="https://tuhis.github.io/gawk/party.html" target="_blank" rel="noreferrer">
          Download gawk-relay (Windows, macOS, Linux)
        </a>
      </p>
      <div className={ui.actions}>
        <button type="button" onClick={onDone}>
          I have copied it
        </button>
      </div>
    </div>
  );
}

function draftOf(p: Pop): BudgetDraft {
  return {
    upMbps: String(p.uplink.maxMbps),
    upStreams: String(p.uplink.maxStreams),
    downMbps: p.downlink.maxMbps ? String(p.downlink.maxMbps) : '',
    downStreams: p.downlink.maxStreams ? String(p.downlink.maxStreams) : '',
    maxBroadcasts: String(p.limits.maxBroadcasts),
  };
}

function PopDetail({ name, namedAddress }: { name: string; namedAddress: boolean }) {
  const api = useApi();
  const load = useCallback(() => api.pop(name), [api, name]);
  const { data: p, error, loading, loadedAt, reload } = useLoader<Pop>(load, 15_000);
  const [confirm, setConfirm] = useState<'repair' | 'revoke' | null>(null);
  const [editing, setEditing] = useState(false);
  const [reveal, setReveal] = useState<PopWithPairing | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  async function act(what: 'repair' | 'revoke') {
    setBusy(true);
    setActionError(null);
    try {
      if (what === 'repair') setReveal(await api.repairPop(name));
      else await api.revokePop(name);
      setConfirm(null);
      reload();
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setActionError(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  const back = (
    <p>
      <a href={href('pops')}>← All PoPs</a>
    </p>
  );
  if (error) {
    return (
      <section>
        {back}
        <p className={ui.error}>{/404|no such/i.test(error) ? `No PoP named ${name}.` : error}</p>
      </section>
    );
  }
  if (!p) return <section>{back}{loading ? <p className={ui.dim}>Loading…</p> : null}</section>;

  const broadcasts = p.broadcasts ?? [];
  const health = p.health ?? [];
  return (
    <section>
      {back}
      <div className={ui.head}>
        <h1>{p.displayName || p.name}</h1>
        <span className={STATE_BADGE[p.state] ?? ui.badge}>{stateText(p, loadedAt)}</span>
        <span className={ui.spacer} />
        <div className={ui.actions}>
          <button type="button" disabled={busy || p.state === 'invalid'} onClick={() => setEditing(true)}>
            Edit
          </button>
          <button type="button" disabled={busy || p.revoked} onClick={() => setConfirm('repair')}>
            Re-pair
          </button>
          <button type="button" className={ui.danger} disabled={busy || p.revoked} onClick={() => setConfirm('revoke')}>
            Revoke
          </button>
        </div>
      </div>
      {actionError && !confirm ? (
        <p className={ui.error} role="alert">
          {actionError}
        </p>
      ) : null}
      {reveal ? <PairingReveal result={reveal} onDone={() => setReveal(null)} /> : null}
      {editing ? (
        <EditPop
          pop={p}
          namedAddress={namedAddress}
          onCancel={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            reload();
          }}
        />
      ) : null}

      {p.live ? (
        <>
          <p>{`${p.live.live} live · ${p.live.forwarded} forwarded · ${p.live.outsideViewers} outside viewers`}</p>
          <UplinkGauge pop={p} />
        </>
      ) : null}

      <h2>Live broadcasts</h2>
      {broadcasts.length === 0 ? (
        <p className={ui.dim}>{p.live ? 'Nothing is live at the PoP.' : 'Not linked: what is live there is unknown.'}</p>
      ) : (
        <div className={ui.tableCard}>
          <table className={ui.table}>
            <thead>
              <tr>
                <th>Label</th>
                <th>Broadcast</th>
                <th>Publisher</th>
                <th>Home</th>
                <th>Viewers</th>
                <th>Wall</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {broadcasts.map((b) => (
                <tr key={b.id}>
                  <td className={ui.breakable}>{b.label ?? ''}</td>
                  <td className={ui.mono}>{b.id}</td>
                  <td className={ui.mono}>{b.publisherLanAddr ?? ''}</td>
                  <td>
                    {b.forwarded ? (
                      <span className={ui.badgeOk}>forwarded</span>
                    ) : (
                      <span className={ui.badge}>party-only</span>
                    )}
                  </td>
                  <td>{`${b.localViewers} here · ${b.outsideViewers} outside`}</td>
                  <td>{b.wall ?? ''}</td>
                  <td>
                    <div className={ui.actions}>
                      {b.links?.watch ? (
                        <a href={b.links.watch} target="_blank" rel="noreferrer">
                          Watch
                        </a>
                      ) : null}
                      <a href={href('broadcasts')} title="Kill and ban from the fleet view">
                        Moderate
                      </a>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className={ui.grid2}>
        <div>
          <h2>Health</h2>
          {health.length === 0 ? (
            <p className={ui.dim}>No health lines.</p>
          ) : (
            <ul>
              {health.map((h) => (
                <li key={h.key}>
                  <span className={h.ok ? ui.badgeOk : ui.badgeBad}>{h.key}</span> <span>{h.detail ?? ''}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
        <div>
          <h2>Addresses</h2>
          <dl className={ui.kv}>
            <dt>Public</dt>
            <dd>
              {(p.link?.publicAddrs ?? []).length === 0 ? (
                <span className={ui.dim}>unknown</span>
              ) : (
                (p.link?.publicAddrs ?? []).map((a) => (
                  <span key={a} className={ui.mono}>
                    {a}
                  </span>
                ))
              )}
            </dd>
            <dt>LAN</dt>
            <dd className={ui.mono}>{p.link?.lanAddr ?? ''}</dd>
            <dt>Named host</dt>
            <dd className={ui.mono}>{p.namedAddress.host ?? (p.namedAddress.enabled ? 'not assigned yet' : 'off')}</dd>
            <dt>Certificate</dt>
            <dd>{p.namedAddress.certificateNotAfter ? `expires ${formatInstant(p.namedAddress.certificateNotAfter)}` : ''}</dd>
            <dt>Link pod</dt>
            <dd className={ui.mono}>{p.link?.pod ?? ''}</dd>
            <dt>Wall</dt>
            <dd className={ui.mono}>{p.wall?.roomCode ?? 'none'}</dd>
            <dt>Pairing</dt>
            <dd>
              {p.pairing.paired
                ? `paired${p.pairing.pairedAt ? ` ${formatInstant(p.pairing.pairedAt)}` : ''}`
                : p.pairing.expiresAt
                  ? `unused, redeemable until ${formatInstant(p.pairing.expiresAt)}`
                  : 'none'}
            </dd>
          </dl>
        </div>
      </div>

      {confirm ? (
        <Dialog
          title={confirm === 'repair' ? `Re-pair ${p.displayName || p.name}` : `Revoke ${p.displayName || p.name}`}
          busy={busy}
          onCancel={() => setConfirm(null)}
        >
          <p className={ui.sub}>
            {confirm === 'repair'
              ? 'The credential the relay paired with stops working at once and its link closes; the relay says “Re-pair needed”. The new pairing string is shown once, right after this.'
              : 'Every link and leg of this PoP is refused, its broadcasts leave home, and it keeps serving its LAN as a standalone relay. Re-pairing a revoked PoP is not possible.'}
          </p>
          {actionError ? (
            <p className={ui.error} role="alert">
              {actionError}
            </p>
          ) : null}
          <div className={ui.actions}>
            <button
              type="button"
              className={confirm === 'repair' ? ui.primary : ui.danger}
              disabled={busy}
              onClick={() => void act(confirm)}
            >
              {confirm === 'repair' ? 'Re-pair now' : 'Revoke now'}
            </button>
            <button type="button" disabled={busy} onClick={() => setConfirm(null)}>
              Cancel
            </button>
          </div>
        </Dialog>
      ) : null}
    </section>
  );
}

function EditPop({
  pop,
  namedAddress,
  onCancel,
  onSaved,
}: {
  pop: Pop;
  namedAddress: boolean;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const api = useApi();
  const initial = draftOf(pop);
  const [displayName, setDisplayName] = useState(pop.displayName ?? '');
  const [draft, setDraft] = useState<BudgetDraft>(initial);
  const [autoSwitch, setAutoSwitch] = useState(pop.guests.autoSwitch);
  const [named, setNamed] = useState(pop.namedAddress.enabled);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function save() {
    setBusy(true);
    setError(null);
    try {
      // Only what changed: a PATCH that restated every field would reset a
      // default to its current value and pin it there.
      const req: PopPatchRequest = {};
      if (displayName.trim() !== (pop.displayName ?? '')) req.displayName = displayName.trim();
      if (draft.upMbps !== initial.upMbps || draft.upStreams !== initial.upStreams) {
        req.uplink = budget(draft.upMbps, draft.upStreams) ?? {};
      }
      if (draft.downMbps !== initial.downMbps || draft.downStreams !== initial.downStreams) {
        req.downlink = budget(draft.downMbps, draft.downStreams) ?? {};
      }
      if (draft.maxBroadcasts !== initial.maxBroadcasts) {
        const max = num(draft.maxBroadcasts);
        req.limits = max !== undefined ? { maxBroadcasts: max } : {};
      }
      if (autoSwitch !== pop.guests.autoSwitch) req.guests = { autoSwitch };
      if (named !== pop.namedAddress.enabled) req.namedAddress = named;
      if (Object.keys(req).length > 0) await api.updatePop(pop.name, req);
      onSaved();
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={ui.panel}>
      <h2>Edit {pop.displayName || pop.name}</h2>
      <div className={ui.field}>
        <label htmlFor="pop-edit-display">Display name</label>
        <input id="pop-edit-display" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
      </div>
      <BudgetFields draft={draft} set={setDraft} />
      <label className={ui.row}>
        <input type="checkbox" checked={autoSwitch} onChange={(e) => setAutoSwitch(e.target.checked)} />
        Auto-switch guests to the party relay
      </label>
      <label className={ui.row}>
        <input
          type="checkbox"
          checked={named}
          disabled={!namedAddress && !named}
          onChange={(e) => setNamed(e.target.checked)}
        />
        Named address
      </label>
      {error ? (
        <p className={ui.error} role="alert">
          {error}
        </p>
      ) : null}
      <div className={ui.actions}>
        <button type="button" className={ui.primary} disabled={busy} onClick={() => void save()}>
          Save
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
