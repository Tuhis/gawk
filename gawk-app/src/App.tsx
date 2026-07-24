import { useEffect, useState } from 'react';
import { parseRoute, type Route } from './routing';
import { LandingPage } from './features/landing/LandingPage';
import { BroadcasterScreen } from './features/broadcaster/BroadcasterScreen';
import { ViewerScreen } from './features/viewer/ViewerScreen';
import { TermsPage } from './features/terms/TermsPage';
import { DebugShell } from './features/debug/DebugShell';
import { DebugIndex } from './features/debug/DebugIndex';
import { BroadcastPage } from './features/stream/BroadcastPage';
import { ViewPage } from './features/stream/ViewPage';
import { LoopbackPage } from './features/loopback/LoopbackPage';

// Hash-based routing (docs/10 Decision 1): production surfaces at #/,
// #/broadcast, #/view/<id>; the frozen diagnostic pages under #/debug/*.
// parseRoute is pure and unit-tested; this shell only subscribes + redirects.
function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parseRoute(window.location.hash));
  useEffect(() => {
    const onChange = () => setRoute(parseRoute(window.location.hash));
    window.addEventListener('hashchange', onChange);
    return () => window.removeEventListener('hashchange', onChange);
  }, []);
  return route;
}

export default function App() {
  const route = useRoute();

  // #/view with no/invalid id, or any unknown route, bounces to the landing
  // page — which owns code entry.
  useEffect(() => {
    if (route.view === 'redirect') window.location.hash = route.to;
  }, [route]);

  switch (route.view) {
    case 'landing':
      return <LandingPage />;
    case 'broadcaster':
      return <BroadcasterScreen />;
    case 'viewer':
      return <ViewerScreen broadcastId={route.broadcastId} />;
    case 'terms':
      return <TermsPage />;
    case 'debug-index':
      return (
        <DebugShell>
          <DebugIndex />
        </DebugShell>
      );
    case 'debug-broadcast':
      return (
        <DebugShell active="#/debug/broadcast">
          <BroadcastPage />
        </DebugShell>
      );
    case 'debug-view':
      return (
        <DebugShell active="#/debug/view">
          <ViewPage />
        </DebugShell>
      );
    case 'debug-loopback':
      return (
        <DebugShell active="#/debug/loopback">
          <LoopbackPage />
        </DebugShell>
      );
    case 'redirect':
      return null;
  }
}
