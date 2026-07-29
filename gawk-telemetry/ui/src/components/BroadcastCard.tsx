import type { BroadcastView } from '../api/types.ts';
import { ago, dur, EMPTY, num, shortId } from '../lib/format.ts';
import { effectiveSeverity } from '../lib/severity.ts';
import { href } from '../router/router.ts';
import { hasEscalated, opensByDefault, useUiStore } from '../state/uiStore.ts';
import { SeverityBadge } from './SeverityBadge.tsx';
import { SessionTable } from './SessionTable.tsx';
import styles from './BroadcastCard.module.css';

interface Props {
  broadcast: BroadcastView;
  ended: boolean;
  found: boolean;
}

/** One fact in the summary strip. Fixed-width value slot: see the CSS. */
function Fact({ label, value }: { label: string; value: string }) {
  return (
    <span className={styles.fact}>
      <span className={styles.factLabel}>{label}</span>
      <span className={`${styles.factValue} tnum`}>{value}</span>
    </span>
  );
}

export function BroadcastCard({ broadcast: b, ended, found }: Props) {
  const worst = effectiveSeverity(b.severity, b.worstViewer);
  const byDefault = opensByDefault(worst);
  const isCardOpen = useUiStore((s) => s.isCardOpen);
  const setCardOpen = useUiStore((s) => s.setCardOpen);
  // Subscribe to the override map so a change re-renders this card.
  useUiStore((s) => s.cardOverrides[b.broadcastKey]);
  const open = isCardOpen(b.broadcastKey, byDefault);

  // UD19's watch, and the whole of it: a starred broadcast pins to the top and
  // VISIBLY changes when its severity escalates. No notification — Chrome
  // throttles a background tab to ~1/min, so an alert could arrive a minute
  // late, which for a stuttering stream is worse than useless.
  const watched = useUiStore((s) => !!s.watched[b.broadcastKey]);
  const toggleWatch = useUiStore((s) => s.toggleWatch);
  const baseline = useUiStore((s) => s.watchSeverity[b.broadcastKey]);
  const observeSeverity = useUiStore((s) => s.observeSeverity);
  if (watched && baseline === undefined) observeSeverity(b.broadcastKey, worst);
  const escalated = watched && hasEscalated(baseline, worst);

  const m = b.metrics ?? {};

  return (
    <section
      className={[
        styles.card,
        worst === 'bad' ? styles.bad : worst === 'warn' ? styles.warn : '',
        found ? styles.found : '',
        escalated ? styles.escalated : '',
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <header className={styles.summary}>
        <button
          type="button"
          className={styles.disclosure}
          aria-expanded={open}
          onClick={() => setCardOpen(b.broadcastKey, !open, byDefault)}
          title={open ? 'Collapse' : 'Expand'}
        >
          {open ? '▾' : '▸'}
        </button>
        <SeverityBadge severity={worst} />
        <button
          type="button"
          className={`${styles.star} ${watched ? styles.starOn : ''}`}
          aria-pressed={watched}
          title={watched ? 'Stop watching this broadcast' : 'Watch: pin it and show me if it gets worse'}
          onClick={() => toggleWatch(b.broadcastKey)}
        >
          {watched ? '★' : '☆'}
        </button>
        <a className={styles.key} href={href('broadcast', b.broadcastKey)} title="Every participant on one axis">
          {shortId(b.broadcastKey)}
        </a>
        {escalated && (
          <span className={styles.escalation}>
            escalated to {worst} since you starred it
          </span>
        )}
        {/*
          Lifecycle is a SEPARATE dimension from severity, and it is spelled in
          words. An ended row's claim is past tense: "is stuttering" and
          "stuttered" are not the same statement, and rendering them identically
          invites acting on a problem that is already over.
        */}
        <span className={`${styles.lifecycle} ${ended ? styles.lifecycleEnded : ''}`}>
          {ended ? `ended ${ago(b.endedAgoMs)}` : b.lifecycle === 'live' ? 'LIVE' : b.lifecycle}
        </span>
        {found && <span className={styles.foundTag}>found</span>}

        <span className={styles.spacer} />

        <span className={styles.facts}>
          <Fact label="viewers" value={String(b.viewers)} />
          {!ended && <Fact label="up" value={dur(b.uptimeMs)} />}
          <Fact
            label="ingress loss"
            value={
              m.ingressLossRatio === undefined ? EMPTY : `${num(m.ingressLossRatio * 100, 2)}%`
            }
          />
          <Fact
            label="egress drops"
            value={m.datagramsDropped === undefined ? EMPTY : num(m.datagramsDropped)}
          />
          {b.pod && <Fact label="pod" value={`${b.pod}${b.role ? ` (${b.role})` : ''}`} />}
        </span>
      </header>

      {b.findings && b.findings.length > 0 && (
        <ul className={styles.findings}>
          {b.findings.map((f) => (
            <li key={f.id}>{f.verdict}</li>
          ))}
        </ul>
      )}

      {open &&
        (b.sessions && b.sessions.length > 0 ? (
          <SessionTable sessions={b.sessions} />
        ) : (
          // A live broadcast with no session rows is a real and interesting
          // state — the relay sees it, nothing has reported. Saying so beats an
          // empty area that reads as a rendering bug.
          <p className={styles.empty}>
            No sessions have reported for this broadcast.
          </p>
        ))}
    </section>
  );
}
