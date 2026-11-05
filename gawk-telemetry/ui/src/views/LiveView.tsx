import { useEffect, useMemo } from 'react';

import type { Severity } from '../api/types.ts';
import { BroadcastCard, EndedRow } from '../components/BroadcastCard.tsx';
import { FindStream } from '../components/FindStream.tsx';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { clockTime } from '../lib/format.ts';
import { effectiveSeverity } from '../lib/severity.ts';
import { href } from '../router/router.ts';
import { useLiveStore } from '../state/liveStore.ts';
import { useUiStore } from '../state/uiStore.ts';
import styles from './LiveView.module.css';
import view from './view.module.css';

// UD13: **the live fleet page stays the landing view.** This is TM8's surface —
// what you open when someone says "it's stuttering", and it does that job.
// History, Explore, Fleet and Rules are peers behind the nav.
//
// R36 (docs/86) restyled it and moved nothing: the counts of what is wrong sit
// beside the title, a watched broadcast still pins to the top of the live
// group, and recently-ended broadcasts are a compact, recessed list below.

export function LiveView() {
  const snapshot = useLiveStore((s) => s.snapshot);
  const foundKey = useUiStore((s) => s.foundKey);
  const watched = useUiStore((s) => s.watched);
  const prune = useUiStore((s) => s.prune);

  const live = useMemo(() => snapshot?.live ?? [], [snapshot]);
  const ended = useMemo(() => snapshot?.ended ?? [], [snapshot]);

  useEffect(() => {
    prune(new Set([...live, ...ended].map((b) => b.broadcastKey)));
  }, [live, ended, prune]);

  // Watched broadcasts pin to the top of the LIVE group only. Pinning inside
  // the ended group would be pointless — nothing there can still be acted on,
  // which is the reason the two groups are never interleaved in the first place.
  const liveOrdered = useMemo(() => {
    const rank = (k: string) => (watched[k] ? 0 : 1);
    return [...live].sort((a, b) => rank(a.broadcastKey) - rank(b.broadcastKey));
  }, [live, watched]);

  const counts = useMemo(() => {
    const c: Record<Severity, number> = { bad: 0, warn: 0, ok: 0, unknown: 0 };
    for (const b of live) c[effectiveSeverity(b.severity, b.worstViewer)] += 1;
    return c;
  }, [live]);
  const viewers = live.reduce((n, b) => n + b.viewers, 0);

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Live</h1>
        <span className={view.subtitle}>
          {snapshot
            ? `${live.length} broadcast${live.length === 1 ? '' : 's'} · ${viewers} viewer${viewers === 1 ? '' : 's'}`
            : 'connecting…'}
        </span>
        {(['bad', 'warn'] as const).map((s) =>
          counts[s] ? (
            <span key={s} className={styles.count}>
              <SeverityBadge severity={s} />
              <span className={view.srOnly}>×</span>
              <b>{counts[s]}</b>
            </span>
          ) : null,
        )}
        <span className={view.spacer} />
        {/* A fixed slot: "connecting…" and "updated 19:12:55" differ in width. */}
        <span className={styles.updated}>{snapshot ? `updated ${clockTime(snapshot.atMs)}` : ''}</span>
        <FindStream />
      </header>

      {/*
        Live first, ALWAYS as its own group. The grouping IS the precedence:
        a live `warn` outranks an ended `bad`, because only the live one can
        still be acted on. The two are never interleaved.
      */}
      <div className={styles.group}>
        <h2>Live</h2>
        <span className={view.note}>watched broadcasts pin to the top</span>
      </div>
      {liveOrdered.length === 0 ? (
        <p className={`${view.panel} ${styles.empty}`}>Nothing is streaming right now.</p>
      ) : (
        liveOrdered.map((b) => <BroadcastCard key={b.broadcastKey} broadcast={b} found={foundKey === b.broadcastKey} />)
      )}

      {ended.length > 0 && (
        <>
          <div className={styles.group}>
            <h2 className={view.dim}>Recently ended</h2>
            <span className={view.note}>
              stored verdicts — nothing here can still be acted on · <a href={href('history')}>History</a> has
              everything older
            </span>
          </div>
          <section className={`${view.panel} ${styles.endedGroup}`} aria-label="Recently ended">
            {ended.map((b) => (
              <EndedRow key={b.broadcastKey} broadcast={b} found={foundKey === b.broadcastKey} />
            ))}
          </section>
        </>
      )}
    </div>
  );
}
