// Test-only helpers. Nothing imports this from app code, so it never reaches
// the bundle — but `tsc -b` still type-checks it, which is the point: the
// stub's shape has to keep up with the real session.

import { render } from '@testing-library/react';
import type { ReactElement } from 'react';

import { AuthProvider } from '../auth/AuthContext.tsx';
import type { AuthSession, SessionState } from '../auth/session.ts';

export interface ApiCall {
  path: string;
  init: RequestInit;
}

export type ApiHandler = (path: string, init: RequestInit) => Response | Promise<Response>;

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

/** Parse the JSON body a view sent, for asserting on what it actually asked for. */
export function bodyOf(call: ApiCall): unknown {
  return JSON.parse(String(call.init.body ?? 'null'));
}

const AUTHENTICATED: SessionState = { status: 'authenticated' };

/**
 * A session stub that records what the views ask for.
 *
 * `getState` returns one stable object on purpose: `useSyncExternalStore`
 * compares snapshots by identity, and a fresh literal per call would loop.
 */
export function stubSession(
  handler: ApiHandler,
  state: SessionState = AUTHENTICATED,
): AuthSession & { calls: ApiCall[]; loginCount: () => number } {
  const calls: ApiCall[] = [];
  let logins = 0;
  const stub = {
    calls,
    loginCount: () => logins,
    authorizedFetch: async (path: string, init: RequestInit = {}) => {
      calls.push({ path, init });
      return handler(path, init);
    },
    subscribe: () => () => undefined,
    getState: () => state,
    accessToken: () => 'test-access-token',
    beginLogin: async () => {
      logins++;
    },
    logout: async () => undefined,
  };
  return stub as unknown as AuthSession & { calls: ApiCall[]; loginCount: () => number };
}

export function renderWithSession(node: ReactElement, session: AuthSession) {
  return render(<AuthProvider session={session}>{node}</AuthProvider>);
}
