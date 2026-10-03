import { useEffect, useState, useSyncExternalStore } from 'react';
import type { ReactNode } from 'react';

import type { AuthSession, SessionState } from '@gawk/oidc-session';

import { fetchMe } from '../api/client.ts';
import type { Me } from '../api/types.ts';
import { bootstrapAuth, useAuthStore } from './auth.ts';
import styles from './AuthGate.module.css';

// What stands between the page and its reads (docs/55 D5).
//
// In none/basic mode it renders its children and nothing else: the dashboard
// is exactly what it was. In OIDC mode the dashboard mounts only once a token
// is held, so no view ever issues a read that has no bearer to carry — and
// the two failure shapes are deliberately different (docs/42 §4.8, which the
// portal's App follows too):
//
//   * **401** — no credential, or an expired one. The session refreshes and,
//     failing that, re-runs the redirect flow. Nothing is rendered for it.
//   * **403** — a VALID token without the reader role. That is a fact at the
//     IdP; bouncing through login would loop for ever and say nothing, so it
//     gets a page that names the role and the identity it was checked against.

const IDLE: SessionState = { status: 'idle' };
const noSubscribe = () => () => {};

function useSessionState(session: AuthSession | null): SessionState {
  return useSyncExternalStore(
    session ? session.subscribe : noSubscribe,
    session ? session.getState : () => IDLE,
    session ? session.getState : () => IDLE,
  );
}

export function AuthGate({ children }: { children: ReactNode }) {
  const mode = useAuthStore((s) => s.mode);
  const session = useAuthStore((s) => s.session);
  const probeError = useAuthStore((s) => s.probeError);
  const state = useSessionState(session);

  if (mode === 'none') return <>{children}</>;
  if (mode === 'pending') return <Centred>Loading…</Centred>;
  if (mode === 'failed') {
    return (
      <Centred>
        <h1>Cannot reach the telemetry service</h1>
        <p className={styles.error}>{probeError}</p>
        <button type="button" onClick={() => void bootstrapAuth()}>
          Try again
        </button>
      </Centred>
    );
  }
  if (!session) return <Centred>Loading…</Centred>;

  switch (state.status) {
    case 'authenticated':
      return <>{children}</>;
    case 'forbidden':
      return <MissingRole session={session} />;
    case 'error':
      return (
        <Centred>
          <h1>Sign-in failed</h1>
          <p className={styles.error}>{state.message ?? 'sign-in failed'}</p>
          <button type="button" onClick={() => void session.beginLogin()}>
            Try again
          </button>
        </Centred>
      );
    case 'idle':
      // Only reachable after Sign out with a provider that offers no
      // end-session endpoint: `start` leaves `idle` before the gate first
      // renders, and nothing else returns to it.
      return (
        <Centred>
          <h1>Signed out</h1>
          <button type="button" onClick={() => void session.beginLogin()}>
            Sign in
          </button>
        </Centred>
      );
    default:
      return <Centred>Signing in…</Centred>;
  }
}

function Centred({ children }: { children: ReactNode }) {
  return <div className={styles.centred}>{children}</div>;
}

/**
 * The 403 page. It names the role because granting it is an IdP action — the
 * dashboard has no identity list to add anyone to — and it names the identity
 * the check ran against, because "signed in as the wrong account" is the
 * other common answer. `/v1/me` needs a valid token but not the role, so it
 * still answers here.
 */
function MissingRole({ session }: { session: AuthSession }) {
  const [me, setMe] = useState<Me | null>(null);
  useEffect(() => {
    const ctrl = new AbortController();
    fetchMe(ctrl.signal).then(setMe, () => {});
    return () => ctrl.abort();
  }, []);
  return (
    <Centred>
      <h1>Not a telemetry reader</h1>
      <p>
        You are signed in
        {me ? (
          <>
            {' '}
            as <strong>{me.email || me.subject}</strong>
          </>
        ) : null}
        , but that identity does not hold the role this deployment requires for its diagnostics —{' '}
        <code>telemetry-reader</code> unless the operator configured another.
      </p>
      {me ? (
        <p className={styles.dim}>
          Roles this token carries:{' '}
          {me.roles?.length ? me.roles.map((r) => <code key={r}>{r} </code>) : 'none'}
        </p>
      ) : null}
      <p className={styles.dim}>
        Roles are managed in the identity provider, not here. Ask an administrator to grant the role,
        then sign in again — it takes effect at your next token refresh.
      </p>
      <button type="button" onClick={() => void session.logout()}>
        Sign out
      </button>
    </Centred>
  );
}
