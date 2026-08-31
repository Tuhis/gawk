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

  // **The views wait for the probe to settle**, and that is a correctness rule,
  // not a loading spinner.
  //
  // `me` carries both the identity this portal just confirmed and the server's
  // configured defaults. A view rendered before it lands shows Kill and
  // Kill + ban for a caller whose `operator` role has not been confirmed yet,
  // and — because a dialog seeds its fields once, when it opens — an operator
  // fast enough to click in that window would get a kill dialog holding the
  // SPA's fallback cooldown instead of the deployment's `-kill-cooldown`. That
  // is a wrong number on an irreversible action, arriving silently.
  //
  // "Settled" deliberately includes failure: a probe that errored (anything but
  // the 403 handled above) still lets the portal through, degraded, with the
  // documented default and the error on screen — a dead `/api/v1/me` should not
  // black out a moderation console.
  const probeSettled = me !== null || error !== null;

  return (
    <div className={styles.app}>
      <header className={styles.header}>
        <a className={styles.title} href={href('broadcasts')}>
          <span className={styles.dot} aria-hidden="true" />
          <span className={styles.wordmark}>gawk</span>
          <span className={styles.tag}>admin</span>
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
        {probeSettled ? (
          <Routed
            view={route.view}
            path={route.path}
            filterKey={route.key}
            cooldownSeconds={cooldown}
          />
        ) : (
          <p className={styles.dim}>Loading…</p>
        )}
      </main>
    </div>
  );
}

function Routed({
  view,
  path,
  filterKey,
  cooldownSeconds,
}: {
  view: ViewName;
  path: string;
  filterKey: string;
  cooldownSeconds: number;
}) {
  switch (view) {
    case 'broadcasts':
      return (
        <BroadcastsView killCooldownSeconds={cooldownSeconds} initialFilter={filterKey} />
      );
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
