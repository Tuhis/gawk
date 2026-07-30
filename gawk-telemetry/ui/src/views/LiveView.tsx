import { useEffect, useMemo } from 'react';

import { BroadcastCard } from '../components/BroadcastCard.tsx';
import { FindStream } from '../components/FindStream.tsx';
import { clockTime } from '../lib/format.ts';
import { isStale, useLiveStore } from '../state/liveStore.ts';
import { useUiStore } from '../state/uiStore.ts';
import styles from './LiveView.module.css';

// UD13: **the live fleet page stays the landing view.** This is TM8's surface,
// essentially unchanged — it is what you open when someone says "it's
// stuttering", and it does that job. R31 moves nothing about it; History,
// Explore, Fleet and Rules became peers behind the nav instead.
//
// Three things did change, all small and all consequences of other chunks:
// the feed can now be SSE (UD22), a starred broadcast pins to the top (UD19),
// and a session row links to a real page instead of expanding a ten-minute
// client-side graph (§1.1).

export function LiveView() {
  const snapshot = useLiveStore((s) => s.snapshot);
  const error = useLiveStore((s) => s.error);
  const lastOkAt = useLiveStore((s) => s.lastOkAt);
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

  const stale = isStale(lastOkAt);

  return (
    <>
      <div className={styles.bar}>
        <span className={styles.updated}>
          {snapshot ? `updated ${clockTime(snapshot.atMs)}` : 'connecting…'}
        </span>
        {/* The banner occupies its slot whether or not it has anything to say,
            so the page below never jumps when the feed hiccups. */}
        <span className={`${styles.feed} ${stale || error ? styles.feedBad : ''}`}>
          {error ? `feed unavailable: ${error}` : stale ? 'feed stale' : ''}
        </span>
        <span className={styles.spacer} />
        <FindStream />
      </div>

      <main className={styles.main}>
        {/*
          Live first, ALWAYS as its own group. The grouping IS the precedence:
          a live `warn` outranks an ended `bad`, because only the live one can
          still be acted on. The two are never interleaved.
        */}
        <h2 className={styles.groupTitle}>Live</h2>
        {liveOrdered.length === 0 ? (
          <p className={styles.empty}>Nothing is streaming right now.</p>
        ) : (
          liveOrdered.map((b) => (
            <BroadcastCard
              key={b.broadcastKey}
              broadcast={b}
              ended={false}
              found={foundKey === b.broadcastKey}
            />
          ))
        )}

        {ended.length > 0 && (
          <>
            <h2 className={styles.groupTitle}>Recently ended</h2>
            <p className={styles.groupNote}>
              Stored verdicts from finished broadcasts. Nothing here can still be acted on —{' '}
              <a href="#/history">History</a> has everything older.
            </p>
            <div className={styles.endedGroup}>
              {ended.map((b) => (
                <BroadcastCard
                  key={b.broadcastKey}
                  broadcast={b}
                  ended
                  found={foundKey === b.broadcastKey}
                />
              ))}
            </div>
          </>
        )}
      </main>
    </>
  );
}
