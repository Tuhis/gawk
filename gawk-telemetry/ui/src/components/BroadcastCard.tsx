import type { BroadcastView } from '../api/types.ts';
import { ago, dur, EMPTY, num, shortId } from '../lib/format.ts';
import { effectiveSeverity } from '../lib/severity.ts';
import { useNarrow } from '../lib/useNarrow.ts';
import { href } from '../router/router.ts';
import { hasEscalated, opensByDefault, useUiStore } from '../state/uiStore.ts';
import { SeverityBadge, SeverityDot } from './SeverityBadge.tsx';
import { PhoneSessions } from './PhoneSessions.tsx';
import { SessionTable } from './SessionTable.tsx';
import { useSparks } from '../lib/useSparks.ts';
import styles from './BroadcastCard.module.css';
import view from '../views/view.module.css';

interface Props {
  broadcast: BroadcastView;
  found: boolean;
}

/** One fact in the summary strip: label over value, a fixed-width slot. */
function Fact({ label, value, sub, left }: { label: string; value: string; sub?: string; left?: boolean }) {
  return (
    <div className={`${styles.fact} ${left ? styles.factLeft : ''}`}>
      <span className={styles.factLabel}>{label}</span>
      <b className={left ? styles.factMono : undefined} title={value}>
        {value}
      </b>
      {sub ? <em>{sub}</em> : null}
    </div>
  );
}

/**
 * A LIVE broadcast (R36, docs/86 TU1/TU5/TU7). A `bad` or `warn` card wears a
 * severity edge; the summary is a strip of facts; the findings are one line
 * each with the rule that fired; the sessions table opens by itself when
 * something is wrong. Ended broadcasts are the compact `EndedRow` instead.
 */
export function BroadcastCard({ broadcast: b, found }: Props) {
  const worst = effectiveSeverity(b.severity, b.worstViewer);
  const byDefault = opensByDefault(worst);
  const isCardOpen = useUiStore((s) => s.isCardOpen);
  const setCardOpen = useUiStore((s) => s.setCardOpen);
  // Subscribe to the override map so a change re-renders this card.
  useUiStore((s) => s.cardOverrides[b.broadcastKey]);
  const open = isCardOpen(b.broadcastKey, byDefault);
  const sparks = useSparks(b.broadcastKey, open && (b.sessions?.length ?? 0) > 0);
  const narrow = useNarrow();

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
  const room = b.sessions?.find((s) => s.roomKey)?.roomKey;

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
      aria-label={`broadcast ${b.broadcastKey}`}
    >
      <header className={styles.head}>
        <button
          type="button"
          className={styles.disc}
          aria-expanded={open}
          aria-label={open ? 'Collapse' : 'Expand'}
          onClick={() => setCardOpen(b.broadcastKey, !open, byDefault)}
        >
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden>
            <path d={open ? 'M1 3l4 4 4-4' : 'M3 1l4 4-4 4'} fill="none" stroke="currentColor" strokeWidth="1.5" />
          </svg>
        </button>
        <SeverityBadge severity={worst} />
        <button
          type="button"
          className={`${styles.star} ${watched ? styles.starOn : ''}`}
          aria-pressed={watched}
          aria-label={watched ? 'Stop watching this broadcast' : 'Watch this broadcast'}
          title={watched ? 'Stop watching this broadcast' : 'Watch: pin it and show me if it gets worse'}
          onClick={() => toggleWatch(b.broadcastKey)}
        >
          <svg width="14" height="14" viewBox="0 0 16 16" fill={watched ? 'currentColor' : 'none'} stroke="currentColor" strokeWidth="1.4" aria-hidden>
            <path d="M8 1.8l1.9 3.9 4.3.6-3.1 3 .7 4.3L8 11.6l-3.8 2 .7-4.3-3.1-3 4.3-.6z" />
          </svg>
        </button>
        <a className={styles.key} href={href('broadcast', b.broadcastKey)} title="Every participant on one axis">
          {b.broadcastKey}
        </a>
        {/*
          Lifecycle is a SEPARATE dimension from severity, and it is spelled in
          words: "is stuttering" and "stuttered" are not the same statement.
        */}
        {b.lifecycle === 'live' ? (
          <span className={styles.live}>
            <i aria-hidden />
            LIVE
          </span>
        ) : (
          <span className={styles.away}>{b.lifecycle.toUpperCase()}</span>
        )}
        {room && (
          <a className={view.tag} href={href('room', room)} title="Every session of this room">
            room {room.slice(0, 6)}
          </a>
        )}
        {found && <span className={view.tagAccent + ' ' + view.tag}>found</span>}
        {escalated && <span className={styles.escalation}>escalated from {baseline} since you starred it</span>}

        <div className={styles.facts}>
          <Fact
            label="viewers"
            value={String(b.viewers)}
            sub={b.stripeLegs ? `+${b.stripeLegs} stripe leg${b.stripeLegs === 1 ? '' : 's'}` : undefined}
          />
          <Fact label="up" value={dur(b.uptimeMs)} />
          <Fact
            label="ingress loss"
            value={m.ingressLossRatio === undefined ? EMPTY : `${num(m.ingressLossRatio * 100, 2)}%`}
          />
          <Fact label="egress drops" value={m.datagramsDropped === undefined ? EMPTY : num(m.datagramsDropped)} />
          <Fact label="pod" value={b.pod ?? EMPTY} sub={b.role} left />
        </div>
      </header>

      {b.findings && b.findings.length > 0 && (
        <ul className={styles.findings}>
          {b.findings.map((f) => (
            <li key={f.id}>
              <SeverityDot severity={f.severity} />
              <span>{f.verdict}</span>
              <a className={view.tag} href={href('rules', undefined, { rule: f.id })}>
                {f.id}
              </a>
            </li>
          ))}
        </ul>
      )}

      {open &&
        (b.sessions && b.sessions.length > 0 ? (
          narrow ? (
            <PhoneSessions sessions={b.sessions} sparks={sparks} />
          ) : (
            <div className={styles.table}>
              <SessionTable sessions={b.sessions} sparks={sparks} />
            </div>
          )
        ) : (
          // A live broadcast with no session rows is a real and interesting
          // state — the relay sees it, nothing has reported. Saying so beats an
          // empty area that reads as a rendering bug.
          <p className={styles.empty}>No sessions have reported for this broadcast.</p>
        ))}
    </section>
  );
}

/**
 * A recently ENDED broadcast, as one compact row. Recessed, because nothing
 * here can still be acted on — but an ended `bad` keeps its red.
 */
export function EndedRow({ broadcast: b, found }: Props) {
  const worst = effectiveSeverity(b.severity, b.worstViewer);
  const verdict = b.findings?.[0]?.verdict;
  return (
    <div className={`${styles.endRow} ${found ? styles.found : ''}`}>
      <span>
        <SeverityBadge severity={worst} />
      </span>
      <a className={styles.endKey} href={href('broadcast', b.broadcastKey)} title={b.broadcastKey}>
        {shortId(b.broadcastKey, 12)}
      </a>
      <span className={styles.ended}>ended {ago(b.endedAgoMs)}</span>
      <span>
        {b.viewers} viewer{b.viewers === 1 ? '' : 's'}
      </span>
      <span className={styles.endVerdict} title={verdict}>
        {verdict ?? EMPTY}
      </span>
    </div>
  );
}
