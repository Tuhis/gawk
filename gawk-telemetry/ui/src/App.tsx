import { useEffect, useRef } from 'react';

import { ClockNote, Nav, PauseBar } from './components/Chrome.tsx';
import { CommandPalette } from './components/CommandPalette.tsx';
import { href, useRoute } from './router/router.ts';
import { useLiveStore } from './state/liveStore.ts';
import { useMetaStore } from './state/metaStore.ts';
import { BroadcastView } from './views/BroadcastView.tsx';
import { ExploreView } from './views/ExploreView.tsx';
import { FleetView } from './views/FleetView.tsx';
import { HistoryView } from './views/HistoryView.tsx';
import { LiveView } from './views/LiveView.tsx';
import { RulesView } from './views/RulesView.tsx';
import { SessionView } from './views/SessionView.tsx';
import { SqlView } from './views/SqlView.tsx';
import styles from './App.module.css';

// The shell. Everything that used to be here is now `views/LiveView.tsx`; what
// remains is what every view shares.
//
// The router (TH1) is what made this a shell rather than a page, and it is a
// CORRECTNESS fix: `#/session/<id>` has been written into every rollup row's
// stored verdict since R28, rollups are permanent, and the SPA had no router —
// so the defect was being written into the one artifact that is never pruned.

export function App() {
  const route = useRoute();
  const start = useLiveStore((s) => s.start);
  const noteGap = useLiveStore((s) => s.noteGap);
  const loadMeta = useMetaStore((s) => s.load);
  const hiddenSince = useRef<number | null>(null);

  useEffect(() => {
    void loadMeta();
  }, [loadMeta]);

  useEffect(() => start(), [start]);

  // TH11's background-throttle honesty. A backgrounded tab is throttled to
  // roughly one timer per minute, so the page genuinely did not observe that
  // window — and it MARKS the gap rather than backfilling or drawing through
  // it. Backfilling would be the worst of both: data the page never had,
  // rendered as though it watched.
  useEffect(() => {
    const onVisibility = () => {
      if (document.hidden) {
        hiddenSince.current = Date.now();
        return;
      }
      const since = hiddenSince.current;
      hiddenSince.current = null;
      if (since && Date.now() - since > 30_000) noteGap(Date.now() - since);
    };
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, [noteGap]);

  return (
    <div className={styles.app}>
      <header className={styles.header}>
        <a className={styles.title} href={href('live')}>
          gawk telemetry
        </a>
        <Nav />
        <span className={styles.spacer} />
        <PauseBar />
        <ClockNote />
      </header>

      <main className={styles.main}>
        <Route route={route} />
      </main>

      <CommandPalette />
    </div>
  );
}

function Route({ route }: { route: ReturnType<typeof useRoute> }) {
  switch (route.view) {
    case 'live':
      return <LiveView />;
    case 'session':
      // The id is passed through even when malformed: the view renders "no such
      // session", which is TH1's criterion. A router that silently fell back to
      // the fleet page would be the original defect wearing a new hat.
      return <SessionView sessionId={route.id ?? ''} />;
    case 'broadcast':
      return <BroadcastView broadcastKey={route.id ?? ''} />;
    case 'history':
      return <HistoryView />;
    case 'explore':
      return <ExploreView />;
    case 'fleet':
      return <FleetView />;
    case 'rules':
      return <RulesView />;
    case 'sql':
      return <SqlView />;
    default:
      return (
        <div className={styles.notFound}>
          <h1>No such view</h1>
          <p>
            <code>{route.raw || '/'}</code> does not name anything here.{' '}
            <a href={href('live')}>Live fleet</a> · <a href={href('history')}>History</a>
          </p>
        </div>
      );
  }
}
