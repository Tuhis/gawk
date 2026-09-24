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
import { applyRouteGrant } from './features/room/grantHandoff';
import { RoomScreen } from './features/room/RoomScreen';
import { JoinResolver } from './features/room/JoinResolver';

// Hash-based routing; parseRoute is pure, this shell subscribes and redirects.
//
// The ?relay= override and the room link's ?rt= grant are applied while the
// route resolves, before its screen mounts: the screens dial on mount and must
// never reach the wrong relay first, and the grant must be stashed (and
// stripped from the URL, so it never survives into a copied link) before the
// room screen needs it.
function resolveRoute(hash: string): Route {
  const route = parseRoute(hash);
  applyRouteRelay(route);
  applyRouteGrant(route);
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
      return <ViewerScreen key={route.broadcastId} broadcastId={route.broadcastId} />;
    case 'room':
      return <RoomScreen key={route.code} code={route.code} />;
    case 'join':
      return <JoinResolver key={route.code} code={route.code} />;
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

  // Detected once per page load, above the route, so a direct #/view/<id>
  // link (the one a first-time visitor opens) warns like the landing page.
  // The acknowledgment is deliberately not persisted: every page load warns
  // again, hash navigation does not.
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
