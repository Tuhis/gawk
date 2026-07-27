import { useEffect, useMemo } from 'react';

import { BroadcastCard } from './components/BroadcastCard.tsx';
import { FindStream } from './components/FindStream.tsx';
import { clockTime } from './lib/format.ts';
import { POLL_MS, useLiveStore } from './state/liveStore.ts';
import { useUiStore } from './state/uiStore.ts';
import styles from './App.module.css';

/**
 * How long after the last successful poll the feed is called stale. Three
 * missed polls: one failure is a blip, three is a fact worth showing.
 */
const STALE_AFTER_MS = POLL_MS * 3;

export function App() {
  const snapshot = useLiveStore((s) => s.snapshot);
  const history = useLiveStore((s) => s.history);
  const error = useLiveStore((s) => s.error);
  const lastOkAt = useLiveStore((s) => s.lastOkAt);
  const poll = useLiveStore((s) => s.poll);
  const foundKey = useUiStore((s) => s.foundKey);
  const prune = useUiStore((s) => s.prune);

  useEffect(() => {
    void poll();
    const id = setInterval(() => void poll(), POLL_MS);
    return () => clearInterval(id);
  }, [poll]);

  const live = useMemo(() => snapshot?.live ?? [], [snapshot]);
  const ended = useMemo(() => snapshot?.ended ?? [], [snapshot]);

  useEffect(() => {
    prune(new Set([...live, ...ended].map((b) => b.broadcastKey)));
  }, [live, ended, prune]);

  // Staleness is computed from the last SUCCESS, not from the error flag: a
  // feed that has been failing for one poll is not yet worth shouting about,
  // and the last good data stays on screen either way.
  const staleMs = lastOkAt === null ? null : Date.now() - lastOkAt;
  const stale = staleMs !== null && staleMs > STALE_AFTER_MS;

  return (
    <div className={styles.app}>
      <header className={styles.header}>
        <h1 className={styles.title}>gawk telemetry</h1>
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
      </header>

      <main className={styles.main}>
        {/*
          Live first, ALWAYS as its own group. The grouping IS the precedence:
          a live `warn` outranks an ended `bad`, because only the live one can
          still be acted on. The two are never interleaved.
        */}
        <h2 className={styles.groupTitle}>Live</h2>
        {live.length === 0 ? (
          <p className={styles.empty}>Nothing is streaming right now.</p>
        ) : (
          live.map((b) => (
            <BroadcastCard
              key={b.broadcastKey}
              broadcast={b}
              ended={false}
              history={history}
              found={foundKey === b.broadcastKey}
            />
          ))
        )}

        {ended.length > 0 && (
          <>
            <h2 className={styles.groupTitle}>Recently ended</h2>
            <p className={styles.groupNote}>
              Stored verdicts from finished broadcasts. Nothing here can still be acted on.
            </p>
            <div className={styles.endedGroup}>
              {ended.map((b) => (
                <BroadcastCard
                  key={b.broadcastKey}
                  broadcast={b}
                  ended
                  history={history}
                  found={foundKey === b.broadcastKey}
                />
              ))}
            </div>
          </>
        )}
      </main>
    </div>
  );
}
