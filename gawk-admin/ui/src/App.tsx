import { useCallback } from 'react';

import { useApi, useSession, useSessionState } from './auth/AuthContext.tsx';
import type { Me } from './api/types.ts';
import { DEFAULT_KILL_COOLDOWN_SECONDS } from './lib/format.ts';
import { useLoader } from './lib/useLoader.ts';
import { href, useRoute } from './router/router.ts';
import type { ViewName } from './router/router.ts';
import { BansView } from './views/BansView.tsx';
import { BroadcastsView } from './views/BroadcastsView.tsx';
import { EventsView } from './views/EventsView.tsx';
import { RelaysView } from './views/RelaysView.tsx';
import { WebhooksView } from './views/WebhooksView.tsx';
import styles from './App.module.css';

const NAV: readonly { view: ViewName; label: string }[] = [
  { view: 'broadcasts', label: 'Broadcasts' },
  { view: 'bans', label: 'Bans' },
  { view: 'events', label: 'Events' },
  { view: 'relays', label: 'Relays' },
  { view: 'webhooks', label: 'Webhooks' },
];

/**
 * The shell.
 *
 * Its first job is the authorization gate, and the two failure shapes are
 * deliberately different (docs/42 §4.8):
 *
 *   * **401** — no credential, or an expired one. The session refreshes and, if
 *     that fails, re-runs the redirect flow. Nothing is rendered here.
 *   * **403** — a VALID token whose identity lacks the `operator` role. That is
 *     an IdP-side fact, so sending the operator back through login would loop
 *     forever and tell them nothing. It gets a page that names the role.
 */
export function App() {
  const state = useSessionState();

  if (state.status === 'forbidden') return <MissingRole />;
  if (state.status === 'error') return <FlowError message={state.message ?? 'sign-in failed'} />;
  if (state.status !== 'authenticated') return <Signing />;
  return <Portal />;
}

function Portal() {
  const api = useApi();
  const session = useSession();
  const route = useRoute();

  // The authorization probe (§4.7). A 403 here has already flipped the session
  // to `forbidden` inside `authorizedFetch`, so App re-renders as MissingRole
  // and this component unmounts; there is nothing left to do with the error.
  const loadMe = useCallback(() => api.me(), [api]);
  const { data: me, error } = useLoader<Me>(loadMe);

  const cooldown = me?.defaults?.killCooldownSeconds ?? DEFAULT_KILL_COOLDOWN_SECONDS;

  return (
    <div className={styles.app}>
      <header className={styles.header}>
        <a className={styles.title} href={href('broadcasts')}>
          gawk admin
        </a>
        <nav className={styles.nav}>
          {NAV.map((item) => (
            <a
              key={item.view}
              className={route.view === item.view ? styles.navActive : styles.navLink}
              href={href(item.view)}
            >
              {item.label}
            </a>
          ))}
        </nav>
        <span className={styles.spacer} />
        {me ? <span className={styles.who}>{me.email || me.subject}</span> : null}
        <button type="button" onClick={() => void session.logout()}>
          Sign out
        </button>
      </header>

      <main className={styles.main}>
        {error ? <p className={styles.error}>{error}</p> : null}
        <Routed view={route.view} path={route.path} cooldownSeconds={cooldown} />
      </main>
    </div>
  );
}

function Routed({
  view,
  path,
  cooldownSeconds,
}: {
  view: ViewName;
  path: string;
  cooldownSeconds: number;
}) {
  switch (view) {
    case 'broadcasts':
      return <BroadcastsView killCooldownSeconds={cooldownSeconds} />;
    case 'bans':
      return <BansView />;
    case 'events':
      return <EventsView />;
    case 'relays':
      return <RelaysView />;
    case 'webhooks':
      return <WebhooksView />;
    default:
      return (
        <section>
          <h1>No such view</h1>
          <p className={styles.dim}>
            <code>{path}</code> is not a portal route.{' '}
            <a href={href('broadcasts')}>Go to broadcasts</a>.
          </p>
        </section>
      );
  }
}

function Signing() {
  return (
    <div className={styles.centred}>
      <p>Signing in…</p>
    </div>
  );
}

/**
 * The 403 page. It names the role because granting it is an IdP action — the
 * portal has no identity list to add anyone to (§4.8), so the only useful thing
 * this page can do is tell the reader what to ask for.
 */
function MissingRole() {
  const session = useSession();
  return (
    <div className={styles.centred}>
      <h1>Not an operator</h1>
      <p>
        You are signed in, but your account does not have the <code>operator</code> role, which
        every action in this portal requires.
      </p>
      <p className={styles.dim}>
        Roles are managed in the identity provider, not here. Ask an administrator to grant the
        role, then sign in again — it takes effect at your next token refresh.
      </p>
      <button type="button" onClick={() => void session.logout()}>
        Sign out
      </button>
    </div>
  );
}

function FlowError({ message }: { message: string }) {
  const session = useSession();
  return (
    <div className={styles.centred}>
      <h1>Sign-in failed</h1>
      <p className={styles.error}>{message}</p>
      <button type="button" onClick={() => void session.beginLogin()}>
        Try again
      </button>
    </div>
  );
}
