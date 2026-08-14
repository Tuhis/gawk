import { useEffect, useState, type ReactElement } from 'react';
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
import { detectBrowserSupport, readBrowserEnv } from './lib/browserSupport';
import { UnsupportedBrowserModal } from './ui/UnsupportedBrowserModal';
import { applyRouteRelay } from './features/servers/relayOverride';

// Hash-based routing (docs/10 Decision 1): production surfaces at #/,
// #/broadcast, #/view/<id>; the frozen diagnostic pages under #/debug/*.
// parseRoute is pure and unit-tested; this shell only subscribes + redirects.
//
// R37 (docs/40 §4.2): the ?relay= session override is applied synchronously
// with route resolution — before the route's screen mounts — because the
// viewer/broadcaster connection effects dial on mount and must never open a
// real connection to the wrong relay first.
function resolveRoute(hash: string): Route {
  const route = parseRoute(hash);
  applyRouteRelay(route);
  return route;
}

function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => resolveRoute(window.location.hash));
  useEffect(() => {
    const onChange = () => setRoute(resolveRoute(window.location.hash));
    window.addEventListener('hashchange', onChange);
    return () => window.removeEventListener('hashchange', onChange);
  }, []);
  return route;
}

function renderRoute(route: Route): ReactElement | null {
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

export default function App() {
  const route = useRoute();

  // Detected once per page load, above the route so a direct #/view/<id> link
  // warns exactly like the landing page does — that link is the one most often
  // opened by someone who has never seen this app before.
  //
  // The acknowledgment is component state and is deliberately NOT persisted:
  // this is a "your stream will probably fail" warning, not a terms acceptance
  // (contrast features/terms/acceptance.ts, which stores its version). Every
  // page load re-warns. It does not re-appear on hash navigation, because that
  // is not a new load and App never unmounts.
  const [support] = useState(() => detectBrowserSupport(readBrowserEnv()));
  const [acknowledged, setAcknowledged] = useState(false);

  // #/view with no/invalid id, or any unknown route, bounces to the landing
  // page — which owns code entry.
  useEffect(() => {
    if (route.view === 'redirect') window.location.hash = route.to;
  }, [route]);

  return (
    <>
      {renderRoute(route)}
      {!support.supported && !acknowledged && (
        <UnsupportedBrowserModal support={support} onContinue={() => setAcknowledged(true)} />
      )}
    </>
  );
}
