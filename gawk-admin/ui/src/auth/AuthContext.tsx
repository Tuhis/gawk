// The session and the API client, handed to the views through context.
//
// Context rather than a module singleton so a test can drive a whole view with
// a stubbed session — the alternative is monkey-patching `fetch` globally,
// which makes "is the bearer header attached?" untestable at the seam that
// actually attaches it.

import { createContext, useContext, useMemo, useSyncExternalStore } from 'react';
import type { ReactNode } from 'react';

import { ApiClient } from '../api/client.ts';
import type { AuthSession, SessionState } from './session.ts';

interface AuthContextValue {
  session: AuthSession;
  api: ApiClient;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ session, children }: { session: AuthSession; children: ReactNode }) {
  const value = useMemo<AuthContextValue>(
    () => ({ session, api: new ApiClient(session) }),
    [session],
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth outside AuthProvider');
  return ctx;
}

export function useSession(): AuthSession {
  return useAuth().session;
}

export function useApi(): ApiClient {
  return useAuth().api;
}

/** The session's observable state — 'forbidden' is what renders the 403 page. */
export function useSessionState(): SessionState {
  const { session } = useAuth();
  return useSyncExternalStore(session.subscribe, session.getState, session.getState);
}
