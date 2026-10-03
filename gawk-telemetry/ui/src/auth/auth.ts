// The dashboard's binding of the shared OIDC session (docs/55 D3, D4, D5).
//
// One bundle serves every auth mode, with no build-time switch. The page asks
// the binary that served it — `GET auth/config` — and:
//
//   * **404** means none or basic mode. Nothing here is engaged: every request
//     is a plain `fetch`, exactly as before OIDC existed (basic mode's
//     credential is the browser's own, attached to same-origin requests
//     without any code here).
//   * **200** `{issuer, clientId, audience}` starts the code+PKCE flow, and
//     from then on every read goes through `authorizedFetch`.
//
// `apiFetch` is the ONE seam the API client and the live stream use, so "is
// the bearer attached?" has exactly one answer per mode. The session itself —
// the redirect, the in-memory tokens, renewal — is `@gawk/oidc-session`, shared
// with gawk-admin; what lives here is only what is the dashboard's own: the
// mode probe, the storage prefix, and the `idp_unavailable` reading of a 401.

import { create } from 'zustand';

import { AuthSession, type SessionDeps, type SessionOptions } from '@gawk/oidc-session';

/**
 * The dashboard's values for the shared session. `auth/config` is RELATIVE, like
 * every other request here, so it resolves against the page on `/`, on a
 * port-forward and under an Ingress sub-path alike; the prefix keeps this
 * SPA's in-flight PKCE record apart from gawk-admin's should the two ever share
 * an origin.
 */
export const SESSION_OPTIONS: SessionOptions = {
  configPath: 'auth/config',
  storageKeyPrefix: 'gawk-telemetry',
};

export type AuthMode =
  /** `auth/config` has not answered yet. Nothing is rendered behind it. */
  | 'pending'
  /** None or basic mode: plain `fetch`, the page as it always was. */
  | 'none'
  /** OIDC mode: the session owns every request. */
  | 'oidc'
  /** The probe itself failed, so the mode is unknown. */
  | 'failed';

interface AuthState {
  mode: AuthMode;
  session: AuthSession | null;
  /** Why the probe failed, when `mode` is `failed`. */
  probeError: string | null;
  /**
   * The read listener answered `401 idp_unavailable`: it has never completed
   * OIDC discovery, so it cannot verify ANY token. Not the operator's fault
   * and not fixed by logging in again — a banner, not a redirect. Cleared by
   * the next authorized request that gets through.
   */
  idpUnavailable: string | null;
}

export const useAuthStore = create<AuthState>(() => ({
  mode: 'pending',
  session: null,
  probeError: null,
  idpUnavailable: null,
}));

/**
 * Thrown in place of a `401 idp_unavailable` response. It is raised from the
 * session's own `fetch` dependency, which is what stops `authorizedFetch`
 * treating it as an expired token: a refresh would be pointless, and the
 * redirect that follows a failed one would bounce the operator through the IdP
 * for a fault that is on the server's side of it.
 */
export class IdpUnavailable extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'IdpUnavailable';
  }
}

/** `{"error":{"code","message"}}`, the read API's error envelope (docs/55 D5). */
export async function errorEnvelope(res: Response): Promise<{ code: string; message: string } | null> {
  try {
    const body = (await res.clone().json()) as { error?: unknown };
    const err = body?.error;
    // OAuth endpoints answer `{"error": "<string>"}`; only the object shape is
    // ours.
    if (!err || typeof err !== 'object') return null;
    const { code, message } = err as { code?: unknown; message?: unknown };
    if (typeof code !== 'string') return null;
    return { code, message: typeof message === 'string' ? message : code };
  } catch {
    return null;
  }
}

function detectingIdpUnavailable(inner: typeof globalThis.fetch): typeof globalThis.fetch {
  return async (input, init) => {
    const res = await inner(input, init);
    if (res.status === 401) {
      const env = await errorEnvelope(res);
      if (env?.code === 'idp_unavailable') throw new IdpUnavailable(env.message);
    }
    return res;
  };
}

export interface BootstrapOptions {
  /** For the probe; the browser's `fetch` by default. */
  fetch?: typeof globalThis.fetch;
  /** Handed to the session; the browser's own by default. */
  sessionDeps?: Partial<SessionDeps>;
  options?: SessionOptions;
}

/**
 * Probe the mode and, in OIDC mode, bring the session up. Never throws: the
 * outcome is the store's `mode`, which is what the gate renders from.
 *
 * Resolves once the session has settled this page load — signed in from a
 * callback, or on its way to the IdP.
 */
export async function bootstrapAuth(opts: BootstrapOptions = {}): Promise<void> {
  const doFetch = opts.fetch ?? ((...args) => globalThis.fetch(...args));
  const options = opts.options ?? SESSION_OPTIONS;
  useAuthStore.setState({ mode: 'pending', probeError: null });
  let res: Response;
  try {
    res = await doFetch(options.configPath, { cache: 'no-store' });
  } catch (e) {
    useAuthStore.setState({ mode: 'failed', probeError: message(e) });
    return;
  }
  if (res.status === 404) {
    useAuthStore.setState({ mode: 'none', session: null });
    return;
  }
  if (!res.ok) {
    // Not 404 and not a config: the mode is genuinely unknown. Guessing "none"
    // would render a page whose every read then fails; guessing "oidc" would
    // bounce to an IdP nobody named. Say so instead.
    useAuthStore.setState({ mode: 'failed', probeError: `GET ${options.configPath}: HTTP ${res.status}` });
    return;
  }
  const isJSON = (res.headers.get('content-type') ?? '').includes('json');
  if (!isJSON) {
    // The dashboard handler answers unknown paths with the document (the SPA
    // fallback), so a read listener that does not route `auth/config` at all
    // returns 200 text/html here. That is a server without OIDC: none mode.
    useAuthStore.setState({ mode: 'none', session: null });
    return;
  }

  const sessionFetch = opts.sessionDeps?.fetch ?? ((...args) => globalThis.fetch(...args));
  const session = new AuthSession(options, {
    ...opts.sessionDeps,
    fetch: detectingIdpUnavailable(sessionFetch),
  });
  // `start` flips the session to `authenticating` before its first await, so
  // the gate never sees an un-started session as a signed-out one.
  const started = session.start();
  useAuthStore.setState({ mode: 'oidc', session });
  await started;
}

/**
 * The one way the dashboard talks to its read API. In OIDC mode it attaches
 * the bearer, renews, and throws `AuthRedirect` when it has started a bounce to
 * the IdP; in every other mode it is `fetch`, untouched.
 */
export async function apiFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const { mode, session } = useAuthStore.getState();
  if (mode !== 'oidc' || !session) return fetch(path, init);
  try {
    const res = await session.authorizedFetch(path, init);
    if (useAuthStore.getState().idpUnavailable !== null && res.status !== 401) {
      useAuthStore.setState({ idpUnavailable: null });
    }
    return res;
  } catch (e) {
    if (e instanceof IdpUnavailable) useAuthStore.setState({ idpUnavailable: e.message });
    throw e;
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
