import { useCallback, useMemo, useState } from 'react';

import { AuthRedirect } from '@gawk/oidc-session';
import { useApi } from '../auth/AuthContext.tsx';
import type { BroadcastsPage, ContentFlag, Timeline } from '../api/types.ts';
import { BanDialog } from '../components/BanDialog.tsx';
import { DecisionPanel } from '../components/DecisionPanel.tsx';
import { KillDialog } from '../components/KillDialog.tsx';
import type { KillRequestDraft } from '../components/KillDialog.tsx';
import { TimelineChart } from '../components/TimelineChart.tsx';
import { describeLabels, describeResolution, formatScore } from '../lib/flags.ts';
import { formatInstant } from '../lib/format.ts';
import { killAndBan, pendingNotice } from '../lib/moderate.ts';
import type { BanRequestDraft } from '../lib/moderate.ts';
import { PATTERN_COPY, defaultIncident, formatSeconds, shortLabel } from '../lib/timeline.ts';
import { useLoader } from '../lib/useLoader.ts';
import { href } from '../router/router.ts';
import ui from '../styles/ui.module.css';
import styles from './BroadcastTimelineView.module.css';

const REFRESH_MS = 10_000;

type Range = 'all' | 'hour' | 'quarter';

const RANGES: readonly { id: Range; label: string; ms: number | null }[] = [
  { id: 'all', label: 'Whole stream', ms: null },
  { id: 'hour', label: 'Last hour', ms: 3_600_000 },
  { id: 'quarter', label: 'Last 15 min', ms: 900_000 },
];

/**
 * One broadcast's moderation timeline (R70, docs/72 D9) — `#/broadcasts/<key>`.
 *
 * The history is the point: the same score means a cutscene when it appears
 * once and banned content when it never goes away, and an operator judging a
 * flag needs to see which. Keyed by the HMAC'd key, so the view outlives the
 * broadcast and its URL never carries a joinable ID.
 *
 * Under 720 px the decision panel becomes a bar fixed to the bottom of the
 * screen (the stylesheet), because the portal's most important use is a phone
 * in someone's hand.
 */
export function BroadcastTimelineView({
  broadcastKey,
  killCooldownSeconds,
  refreshMs = REFRESH_MS,
}: {
  broadcastKey: string;
  killCooldownSeconds: number;
  refreshMs?: number;
}) {
  const api = useApi();
  const [range, setRange] = useState<Range>('all');
  const loadTimeline = useCallback(() => {
    const ms = RANGES.find((r) => r.id === range)?.ms ?? null;
    return api.timeline(broadcastKey, ms ? { from: new Date(Date.now() - ms).toISOString() } : {});
  }, [api, broadcastKey, range]);
  const { data, error, loadedAt, reload } = useLoader<Timeline>(loadTimeline, refreshMs);
  const loadFleet = useCallback(() => api.broadcasts(), [api]);
  const fleet = useLoader<BroadcastsPage>(loadFleet, refreshMs);

  const [picked, setPicked] = useState<number | null>(null);
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set());
  const [note, setNote] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);
  const [menu, setMenu] = useState(false);
  const [dialog, setDialog] = useState<'kill' | 'ban' | null>(null);
  const [busy, setBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);

  const broadcasts = fleet.data?.broadcasts ?? [];
  const broadcast = broadcasts.find((b) => b.key === broadcastKey) ?? null;

  const selected: ContentFlag | null = useMemo(() => {
    if (!data) return null;
    return data.incidents.find((f) => f.id === picked) ?? defaultIncident(data.incidents);
  }, [data, picked]);

  function changed(n: string | null, w: string | null) {
    setNote(n);
    setWarning(w);
    reload();
    fleet.reload();
  }

  async function confirmKill(draft: KillRequestDraft) {
    if (!broadcast) return;
    setBusy(true);
    setDialogError(null);
    try {
      const { ban } = await api.kill(broadcast.id, { reason: draft.reason, cooldownSeconds: draft.cooldownSeconds });
      setDialog(null);
      changed(pendingNotice(broadcast.id, ban) ? null : `Ended ${broadcast.id}.`, pendingNotice(broadcast.id, ban));
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setDialogError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function confirmBan(draft: BanRequestDraft) {
    if (!broadcast) return;
    setBusy(true);
    setDialogError(null);
    try {
      const out = await killAndBan(api, broadcast.id, draft);
      if (out.failure !== null) {
        setDialogError(out.failure);
        changed(null, out.pending);
        return;
      }
      setDialog(null);
      changed(out.pending ? null : out.summary, out.pending);
    } catch (err) {
      if (err instanceof AuthRedirect) return;
      setDialogError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  if (!data) {
    return (
      <section>
        <Breadcrumb broadcastKey={broadcastKey} />
        {error ? <p className={ui.error}>{error}</p> : <p className={ui.dim}>Loading the timeline…</p>}
      </section>
    );
  }

  // The ages are measured from the fetch, as the fleet view's are.
  const now = loadedAt ?? Date.parse(data.to);
  const s = data.summary;
  const pattern = PATTERN_COPY[s.pattern];
  const sampled = s.sampledSeconds;
  const abovePct = sampled > 0 ? Math.round((s.aboveSeconds / sampled) * 100) : 0;
  const levels = data.levels;
  const rule =
    levels.threshold != null
      ? `threshold ${formatScore(levels.threshold)}${levels.window && levels.windowHits ? ` · trips at ${levels.windowHits} of ${levels.window}` : ''}`
      : 'the sampler reported no threshold';
  const watch = data.links?.watch ?? null;

  return (
    <section className={styles.view}>
      <Breadcrumb broadcastKey={broadcastKey} />
      <header className={styles.header}>
        <div className={styles.identity}>
          <div className={styles.titleRow}>
            <h1 className={ui.mono}>{data.broadcastId ?? broadcastKey}</h1>
            {data.live ? <span className={ui.badgeBad}>LIVE</span> : <span className={ui.badge}>not live</span>}
          </div>
          <p className={ui.sub}>
            key <code className={ui.mono}>{broadcastKey}</code> · {data.sessions.length} publisher session
            {data.sessions.length === 1 ? '' : 's'} · {formatInstant(data.from)} – {formatInstant(data.to)}
          </p>
        </div>
        <div className={ui.actions}>
          {watch ? (
            <a className={styles.headerButton} href={watch} target="_blank" rel="noreferrer">
              Watch
            </a>
          ) : null}
          <div className={styles.menuWrap}>
            <button type="button" aria-haspopup="menu" aria-expanded={menu} disabled={!broadcast} onClick={() => setMenu(!menu)}
              title={broadcast ? undefined : 'The broadcast is not live'}>
              Broadcast actions ▾
            </button>
            {menu && broadcast ? (
              <div className={styles.menu} role="menu">
                <button type="button" role="menuitem" onClick={() => { setMenu(false); setDialog('kill'); }}>
                  End broadcast…
                </button>
                <button type="button" role="menuitem" className={ui.danger} onClick={() => { setMenu(false); setDialog('ban'); }}>
                  End &amp; ban…
                </button>
              </div>
            ) : null}
          </div>
        </div>
      </header>

      {note ? <p className={ui.ok} role="status">{note}</p> : null}
      {warning ? <p className={ui.warning} role="status">{warning}</p> : null}
      {error ? <p className={ui.error}>{error}</p> : null}

      <div className={styles.layout}>
        <div className={styles.main}>
          <section aria-label="Signal summary" className={styles.tiles}>
            <div className={styles.tile}>
              <span className={styles.tileLabel}>Pattern</span>
              <span className={`${styles.tileValue} ${pattern.tone === 'bad' ? styles.toneBad : pattern.tone === 'warn' ? styles.toneWarn : ''}`}>
                {pattern.label}
              </span>
              <span className={styles.tileSub}>{pattern.detail}</span>
            </div>
            <div className={styles.tile}>
              <span className={styles.tileLabel}>Above threshold</span>
              <span className={styles.tileValue}>{s.thresholdKnown ? `${abovePct}%` : '—'}</span>
              <span className={styles.tileSub}>
                {formatSeconds(s.aboveSeconds)} of {formatSeconds(sampled)} sampled
              </span>
            </div>
            <div className={styles.tile}>
              <span className={styles.tileLabel}>Longest run</span>
              <span className={styles.tileValue}>{formatSeconds(s.longestRunSeconds)}</span>
              <span className={styles.tileSub}>
                {s.runs} run{s.runs === 1 ? '' : 's'} · at critical {formatSeconds(s.criticalSeconds)}
              </span>
            </div>
            <div className={styles.tile}>
              <span className={styles.tileLabel}>Peak</span>
              <span className={styles.tileValue}>{formatScore(s.peak)}</span>
              <span className={`${styles.tileSub} ${ui.mono}`}>{s.peakLabel ? shortLabel(s.peakLabel) : '—'}</span>
            </div>
          </section>

          <section aria-labelledby="tl-title" className={styles.card}>
            <div className={styles.cardHead}>
              <div>
                <h2 id="tl-title">Screening timeline</h2>
                <p className={ui.sub}>
                  1 bar = {formatSeconds(data.bucketSeconds)}, the highest score among the flagged labels · {rule}
                </p>
              </div>
              <div role="group" aria-label="Time range" className={styles.segmented}>
                {RANGES.map((r) => (
                  <button key={r.id} type="button" aria-pressed={range === r.id} className={range === r.id ? styles.segOn : styles.seg} onClick={() => setRange(r.id)}>
                    {r.label}
                  </button>
                ))}
              </div>
            </div>
            {data.buckets.length === 0 && data.sessions.length === 0 ? (
              <p className={ui.dim}>
                Nothing was sampled for this broadcast in this range. Samples are kept for the deployment's retention
                period (30 days by default).
              </p>
            ) : (
              <TimelineChart
                timeline={data}
                hidden={hidden}
                onToggleLabel={(l) => {
                  const next = new Set(hidden);
                  if (next.has(l)) next.delete(l);
                  else next.add(l);
                  setHidden(next);
                }}
                selected={selected?.id ?? null}
                onSelect={(f) => setPicked(f.id)}
              />
            )}
            {data.truncated ? <p className={ui.caveat}>This range holds more samples than one read returns; narrow it to see the newest.</p> : null}
            <Legend />
          </section>

          <section aria-labelledby="inc-title" className={styles.card}>
            <h2 id="inc-title">Incidents</h2>
            {data.incidents.length === 0 ? (
              <p className={ui.dim}>No content flag was raised on this broadcast in this range.</p>
            ) : (
              <ul className={styles.incidents}>
                {[...data.incidents].reverse().map((f) => (
                  <li key={f.id}>
                    <button type="button" aria-pressed={selected?.id === f.id} className={selected?.id === f.id ? styles.incidentRowOn : styles.incidentRow} onClick={() => setPicked(f.id)}>
                      <span className={styles.incidentTop}>
                        <strong>
                          #{f.id}
                          {f.mode === 'observe' ? ' · observed' : ''}
                        </strong>
                        <span className={ui.dim}>{formatInstant(f.raisedAt)}</span>
                        <span className={ui.spacer} />
                        <span className={f.state === 'open' ? ui.badgeBad : f.state === 'acknowledged' ? ui.badgeWarn : ui.badge}>
                          {f.state === 'resolved' ? describeResolution(f) : f.state}
                        </span>
                      </span>
                      <span className={ui.sub}>
                        {f.returnedAfterReview ? 'Returned after review · ' : ''}
                        peak {formatScore(f.peakScore)}
                        {f.trips && f.trips > 1 ? ` · ${f.trips} trips, one page` : ''} · {describeLabels(f)}
                        {f.reminders > 0 ? ` · re-paged ${f.reminders}×` : ''}
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </section>

          {data.events.length > 0 ? (
            <section aria-labelledby="ev-title" className={styles.card}>
              <h2 id="ev-title">Events</h2>
              <ol className={styles.events}>
                {data.events.map((e) => (
                  <li key={e.id}>
                    <span className={ui.dim}>{formatInstant(e.occurredAt)}</span>
                    <span>{e.summary || e.type}</span>
                  </li>
                ))}
              </ol>
            </section>
          ) : null}
          <p className={ui.dim}>No frames are kept — scores and times only. To judge what is on screen, watch the broadcast live.</p>
        </div>

        {selected ? (
          <DecisionPanel
            key={selected.id}
            flag={selected}
            timeline={data}
            broadcast={broadcast}
            broadcasts={broadcasts}
            killCooldownSeconds={killCooldownSeconds}
            now={now}
            onChanged={changed}
          />
        ) : null}
      </div>

      {dialog === 'kill' && broadcast ? (
        <KillDialog
          broadcast={broadcast}
          defaultCooldownSeconds={killCooldownSeconds}
          busy={busy}
          error={dialogError}
          onCancel={() => setDialog(null)}
          onConfirm={(d) => void confirmKill(d)}
        />
      ) : null}
      {dialog === 'ban' && broadcast ? (
        <BanDialog
          broadcast={broadcast}
          broadcasts={broadcasts}
          busy={busy}
          error={dialogError}
          onCancel={() => setDialog(null)}
          onConfirm={(d) => void confirmBan(d)}
        />
      ) : null}
    </section>
  );
}

function Breadcrumb({ broadcastKey }: { broadcastKey: string }) {
  return (
    <nav aria-label="Breadcrumb" className={styles.breadcrumb}>
      <a href={href('broadcasts')}>Broadcasts</a>
      <span aria-hidden="true">/</span>
      <span className={ui.mono}>{broadcastKey}</span>
    </nav>
  );
}

function Legend() {
  return (
    <div className={styles.legend}>
      <span><i className={styles.swCritical} aria-hidden="true" />at or above critical</span>
      <span><i className={styles.swAbove} aria-hidden="true" />above threshold</span>
      <span><i className={styles.swBelow} aria-hidden="true" />above floor</span>
      <span><i className={styles.swSampled} aria-hidden="true" />sampled, below floor</span>
      <span><i className={styles.swNotSampled} aria-hidden="true" />not sampled</span>
      <span><i className={styles.swThreshold} aria-hidden="true" />threshold</span>
      <span><i className={styles.swCriticalLine} aria-hidden="true" />critical</span>
    </div>
  );
}
