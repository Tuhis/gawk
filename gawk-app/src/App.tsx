import { useEffect, useState } from 'react';
import styles from './App.module.css';
import { LoopbackPage } from './features/loopback/LoopbackPage';
import { BroadcastPage } from './features/stream/BroadcastPage';
import { ViewPage } from './features/stream/ViewPage';

// Hash-based routing — three pages don't warrant a router dependency.
const ROUTES = [
  ['#/broadcast', 'Broadcast'],
  ['#/view', 'View'],
  ['#/loopback', 'Loopback'],
] as const;

function useHashRoute(): string {
  const [route, setRoute] = useState(() => window.location.hash);
  useEffect(() => {
    const onChange = () => setRoute(window.location.hash);
    window.addEventListener('hashchange', onChange);
    return () => window.removeEventListener('hashchange', onChange);
  }, []);
  return route;
}

export default function App() {
  const route = useHashRoute();

  let page;
  switch (route) {
    case '#/broadcast':
      page = <BroadcastPage />;
      break;
    case '#/loopback':
      page = <LoopbackPage />;
      break;
    default:
      // Viewing is what 14 of the 15 users are here for.
      page = <ViewPage />;
  }

  return (
    <>
      <nav className={styles.nav}>
        <span className={styles.brand}>gawk</span>
        {ROUTES.map(([hash, label]) => (
          <a
            key={hash}
            href={hash}
            className={
              route === hash || (hash === '#/view' && route !== '#/broadcast' && route !== '#/loopback')
                ? styles.active
                : undefined
            }
          >
            {label}
          </a>
        ))}
      </nav>
      {page}
    </>
  );
}
