// The portal's OIDC public-client session (docs/42 §4.8, AP6).
//
// Hand-rolled authorization-code + PKCE rather than `oidc-client-ts`, and the
// reasons are properties this file has to guarantee rather than configure:
//
//   * **Tokens live in memory only.** `oidc-client-ts`'s `WebStorageStateStore`
//     defaults to `localStorage`, so keeping tokens out of it is a setting an
//     upgrade could quietly change. Here there is no code path that can write a
//     token anywhere: `tokens` is a private field and nothing serializes it.
//   * **Renewal is refresh-token rotation, not a hidden iframe.** That library's
//     silent renew loads the IdP in an iframe; the portal's CSP is
//     `default-src 'self'; connect-src 'self' <issuer>` (§4.8), which sanctions
//     the IdP for XHR and nothing else — the iframe would be blocked. §4.8 asks
//     for refresh-token rotation anyway, which is a single POST.
//   * 124 KB of ESM plus a second dependency for four HTTP calls, of which we
//     would use maybe a twentieth.
//
// What DOES cross the redirect, and therefore cannot be in memory, is the
// transient PKCE record: `state`, `code_verifier`, `nonce` and the hash route
// to return to. It goes in `sessionStorage` under one key and is deleted the
// instant the code is exchanged. None of it is a token, none of it is a
// credential after the exchange, and `localStorage` is never touched at all —
// asserted in `session.test.ts`.

import { codeChallengeS256, randomToken } from './pkce.ts';

/** The unauthenticated bootstrap document, `GET /auth/config` (§4.8). */
export interface AuthConfig {
  issuer: string;
  clientId: string;
  audience: string;
}

/** The subset of the provider's discovery document the flow needs. */
interface ProviderMetadata {
  authorization_endpoint: string;
  token_endpoint: string;
  end_session_endpoint?: string;
}

export type SessionStatus =
  /** Nothing has happened yet. */
  | 'idle'
  /** Bootstrapping, exchanging a code, or on the way to the IdP. */
  | 'authenticating'
  /** Holding a live access token. */
  | 'authenticated'
  /** A VALID token that lacks the operator role — a 403, not a login loop. */
  | 'forbidden'
  /** The flow itself failed; the message is for a human. */
  | 'error';

export interface SessionState {
  status: SessionStatus;
  message?: string;
}

/**
 * Thrown by `authorizedFetch` when the request could not be completed and a
 * full-page redirect to the IdP has been started instead. Callers must treat it
 * as "this page is going away", not as an error to render.
 */
export class AuthRedirect extends Error {
  constructor() {
    super('redirecting to the identity provider');
    this.name = 'AuthRedirect';
  }
}

/** The transient record that has to survive the trip to the IdP and back. */
interface FlowRecord {
  state: string;
  verifier: string;
  nonce: string;
  /** The hash route the operator was on, restored after the exchange. */
  returnTo: string;
  redirectUri: string;
}

/**
 * The one storage key this module ever writes. Exported so the test can assert
 * on it by name rather than by grepping — "nothing else is stored" is the
 * property, and naming the exception makes it checkable.
 */
export const FLOW_STORAGE_KEY = 'gawk-admin.oidc-flow';

/**
 * Scope. Deliberately WITHOUT `offline_access`: §4.8 specifies ordinary
 * session-bound refresh tokens, so that ending the IdP session ends portal
 * access at the next refresh. An `offline_access` grant would outlive the
 * session and blunt exactly the revocation horizon the design leans on.
 */
const SCOPE = 'openid profile email';

/** Everything the session touches outside itself, so a test can supply it. */
export interface SessionDeps {
  fetch: typeof globalThis.fetch;
  now: () => number;
  setTimer: (fn: () => void, ms: number) => number;
  clearTimer: (id: number) => void;
  /** Full-page navigation to the IdP. */
  redirect: (url: string) => void;
  /** Where the transient PKCE record lives across the redirect. */
  storage: Storage;
  /** The current URL, read at bootstrap to detect the callback. */
  currentUrl: () => string;
  /** Replace the URL without navigating — used to strip `?code=…` (§4.8). */
  replaceUrl: (url: string) => void;
}

function browserDeps(): SessionDeps {
  return {
    fetch: (...args) => globalThis.fetch(...args),
    now: () => Date.now(),
    setTimer: (fn, ms) => globalThis.setTimeout(fn, ms) as unknown as number,
    clearTimer: (id) => globalThis.clearTimeout(id),
    redirect: (url) => {
      window.location.assign(url);
    },
    storage: window.sessionStorage,
    currentUrl: () => window.location.href,
    replaceUrl: (url) => window.history.replaceState(null, '', url),
  };
}

interface Tokens {
  accessToken: string;
  refreshToken: string | null;
  /** Epoch milliseconds. */
  expiresAt: number;
}

interface TokenResponse {
  access_token: string;
  refresh_token?: string;
  id_token?: string;
  token_type?: string;
  expires_in?: number;
}

/**
 * How early to renew, as a fraction of the access token's lifetime, clamped so
 * that neither a 30-second test token nor a one-hour production token gets a
 * silly lead time. §4.8 recommends 5–15 minute tokens; a 5-minute token renews
 * a minute early.
 */
function renewalDelayMs(lifetimeMs: number): number {
  const lead = Math.min(60_000, Math.max(5_000, lifetimeMs * 0.2));
  return Math.max(1_000, lifetimeMs - lead);
}

export class AuthSession {
  private readonly deps: SessionDeps;

  // The ONLY home of any token. Private, never serialized, gone on reload —
  // which is why a reload re-runs the redirect flow (§4.8: against a live IdP
  // session that is an invisible bounce).
  private tokens: Tokens | null = null;

  private config: AuthConfig | null = null;
  private meta: ProviderMetadata | null = null;
  private renewTimer: number | null = null;
  private renewInflight: Promise<Tokens | null> | null = null;
  private state: SessionState = { status: 'idle' };
  private readonly listeners = new Set<() => void>();

  constructor(deps: Partial<SessionDeps> = {}) {
    this.deps = { ...browserDeps(), ...deps };
  }

  // --- observable state (useSyncExternalStore) ------------------------------

  getState = (): SessionState => this.state;

  subscribe = (cb: () => void): (() => void) => {
    this.listeners.add(cb);
    return () => {
      this.listeners.delete(cb);
    };
  };

  private setState(status: SessionStatus, message?: string) {
    if (this.state.status === status && this.state.message === message) return;
    this.state = message === undefined ? { status } : { status, message };
    for (const l of this.listeners) l();
  }

  /** The current access token, or null. Exposed for tests and diagnostics. */
  accessToken(): string | null {
    return this.tokens?.accessToken ?? null;
  }

  /** The bootstrap document, once fetched. The views use nothing from it. */
  authConfig(): AuthConfig | null {
    return this.config;
  }

  // --- bootstrap ------------------------------------------------------------

  /**
   * Bring the session up: fetch `/auth/config`, complete a callback if this
   * load is one, otherwise start the redirect flow.
   *
   * Safe to call once per page load. It never throws — every failure lands in
   * the observable state, because there is no one above it to render an error.
   */
  async start(): Promise<void> {
    this.setState('authenticating');
    try {
      const url = new URL(this.deps.currentUrl());
      const error = url.searchParams.get('error');
      if (error) {
        // The IdP refused before we ever got a code. Say so rather than
        // bouncing straight back to it, which would spin.
        this.clearFlow();
        this.deps.replaceUrl(url.origin + url.pathname + url.hash);
        this.setState('error', url.searchParams.get('error_description') ?? error);
        return;
      }
      await this.loadConfig();
      const code = url.searchParams.get('code');
      const state = url.searchParams.get('state');
      if (code && state) {
        await this.completeCallback(url, code, state);
        return;
      }
      await this.beginLogin();
    } catch (err) {
      this.setState('error', message(err));
    }
  }

  private async loadConfig(): Promise<AuthConfig> {
    if (this.config) return this.config;
    // Relative, like every other same-origin request: the page is served by the
    // binary that answers this, so it works on `/`, on a port-forward and under
    // an Ingress sub-path alike.
    const res = await this.deps.fetch('auth/config', { cache: 'no-store' });
    if (!res.ok) throw new Error(`GET /auth/config failed: HTTP ${res.status}`);
    const cfg = (await res.json()) as AuthConfig;
    if (!cfg.issuer || !cfg.clientId) {
      throw new Error('/auth/config is missing issuer or clientId');
    }
    this.config = cfg;
    return cfg;
  }

  private async discover(): Promise<ProviderMetadata> {
    if (this.meta) return this.meta;
    const cfg = await this.loadConfig();
    const base = cfg.issuer.replace(/\/+$/, '');
    const res = await this.deps.fetch(`${base}/.well-known/openid-configuration`, {
      cache: 'no-store',
    });
    if (!res.ok) throw new Error(`OIDC discovery failed: HTTP ${res.status}`);
    const meta = (await res.json()) as ProviderMetadata;
    if (!meta.authorization_endpoint || !meta.token_endpoint) {
      throw new Error('OIDC discovery document is missing an endpoint');
    }
    this.meta = meta;
    return meta;
  }

  // --- the redirect flow ----------------------------------------------------

  /**
   * Start the authorization-code + PKCE redirect. `returnTo` defaults to the
   * hash route currently on screen, so a deep link survives the login bounce —
   * which matters because the webhook `portalUrl` IS a deep link (§4.10).
   */
  async beginLogin(returnTo?: string): Promise<void> {
    this.setState('authenticating');
    const cfg = await this.loadConfig();
    const meta = await this.discover();
    const here = new URL(this.deps.currentUrl());
    // The redirect URI is the page itself with no query and no fragment: it has
    // to match what is registered at the IdP byte for byte, and a fragment is
    // never sent to a server anyway.
    const redirectUri = here.origin + here.pathname;

    const record: FlowRecord = {
      state: randomToken(),
      verifier: randomToken(),
      nonce: randomToken(),
      returnTo: returnTo ?? here.hash,
      redirectUri,
    };
    this.deps.storage.setItem(FLOW_STORAGE_KEY, JSON.stringify(record));

    const params = new URLSearchParams({
      response_type: 'code',
      client_id: cfg.clientId,
      redirect_uri: redirectUri,
      scope: SCOPE,
      state: record.state,
      nonce: record.nonce,
      code_challenge: await codeChallengeS256(record.verifier),
      code_challenge_method: 'S256',
    });
    // An extension parameter some providers need to mint a JWT for a named API
    // rather than for themselves. RFC 6749 §3.1 requires an authorization
    // server to ignore parameters it does not recognize, so sending it is safe
    // for the providers that do not (Keycloak, Authelia, authentik).
    if (cfg.audience) params.set('audience', cfg.audience);

    this.deps.redirect(`${meta.authorization_endpoint}?${params.toString()}`);
  }

  private readFlow(): FlowRecord | null {
    const raw = this.deps.storage.getItem(FLOW_STORAGE_KEY);
    if (!raw) return null;
    try {
      return JSON.parse(raw) as FlowRecord;
    } catch {
      return null;
    }
  }

  private clearFlow() {
    this.deps.storage.removeItem(FLOW_STORAGE_KEY);
  }

  private async completeCallback(url: URL, code: string, state: string): Promise<void> {
    const flow = this.readFlow();
    // Deleted before anything else can fail: a verifier that has been offered
    // once must never be offered again, and a stale record would make the next
    // load look like a callback.
    this.clearFlow();
    // Strip `?code=…&state=…` from the address bar whatever happens next, so a
    // reload is a fresh login rather than a replay of a spent code.
    this.deps.replaceUrl(url.origin + url.pathname + (flow?.returnTo || url.hash));

    if (!flow) {
      this.setState('error', 'no authorization request is in progress');
      return;
    }
    if (state !== flow.state) {
      // The one check that makes the callback ours. Mismatch means the response
      // belongs to some other request — refuse it, do not retry into a loop.
      this.setState('error', 'authorization response did not match the request');
      return;
    }

    const cfg = await this.loadConfig();
    const body = new URLSearchParams({
      grant_type: 'authorization_code',
      code,
      redirect_uri: flow.redirectUri,
      client_id: cfg.clientId,
      code_verifier: flow.verifier,
    });
    // No `client_secret`: the SPA is a public client and no secret exists
    // anywhere in the system (§4.8). PKCE is what binds the code to this
    // browser instead.
    const tokens = await this.tokenRequest(body);
    if (tokens.idToken) {
      const nonce = idTokenNonce(tokens.idToken);
      if (nonce !== undefined && nonce !== flow.nonce) {
        this.setState('error', 'id_token nonce did not match the request');
        return;
      }
    }
    this.adopt(tokens);
  }

  // --- tokens ---------------------------------------------------------------

  private async tokenRequest(
    body: URLSearchParams,
  ): Promise<Tokens & { idToken: string | null }> {
    const meta = await this.discover();
    const res = await this.deps.fetch(meta.token_endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: body.toString(),
      cache: 'no-store',
    });
    if (!res.ok) {
      throw new Error(`token endpoint returned HTTP ${res.status}`);
    }
    const json = (await res.json()) as TokenResponse;
    if (!json.access_token) throw new Error('token response carried no access_token');
    // A provider that omits `expires_in` is treated as short-lived rather than
    // eternal: renewing too often is free, renewing too late is a 401 storm.
    const lifetimeMs = (json.expires_in ?? 300) * 1000;
    return {
      accessToken: json.access_token,
      refreshToken: json.refresh_token ?? null,
      idToken: json.id_token ?? null,
      expiresAt: this.deps.now() + lifetimeMs,
    };
  }

  private adopt(tokens: Tokens) {
    this.tokens = { ...tokens };
    this.setState('authenticated');
    this.scheduleRenewal();
  }

  private scheduleRenewal() {
    if (this.renewTimer !== null) this.deps.clearTimer(this.renewTimer);
    this.renewTimer = null;
    const tokens = this.tokens;
    // Nothing to renew with. The next 401 falls back to the redirect flow,
    // which is the documented behaviour when refresh is unavailable.
    if (!tokens?.refreshToken) return;
    const lifetime = tokens.expiresAt - this.deps.now();
    this.renewTimer = this.deps.setTimer(() => {
      this.renewTimer = null;
      void this.silentRenew();
    }, renewalDelayMs(lifetime));
  }

  /**
   * The scheduled renewal. On failure it goes straight to the full redirect
   * flow (§4.8) rather than waiting for the next API call to 401 — the operator
   * is more likely to be looking at the page now than mid-action later.
   */
  private async silentRenew(): Promise<void> {
    const renewed = await this.tryRenew();
    if (renewed) return;
    this.abandon();
    await this.beginLogin().catch((err) => this.setState('error', message(err)));
  }

  /**
   * Give up on the credential we are holding.
   *
   * Always called immediately before falling back to the redirect flow. The
   * access token may still have a minute of life in it, but renewal has failed
   * or the server has rejected it twice — continuing to send it would produce a
   * page that half-works until it abruptly does not. Dropping it makes the
   * state honest: unauthenticated, and on the way to the IdP.
   */
  private abandon() {
    this.tokens = null;
    if (this.renewTimer !== null) this.deps.clearTimer(this.renewTimer);
    this.renewTimer = null;
  }

  /**
   * Refresh once, single-flighted. Returns null when refresh is impossible or
   * refused — the caller decides what that means.
   *
   * Rotation (§4.8): the response's `refresh_token` REPLACES the stored one. A
   * rotating IdP invalidates the old one on use, so keeping it would guarantee
   * the next refresh fails.
   */
  private tryRenew(): Promise<Tokens | null> {
    if (this.renewInflight) return this.renewInflight;
    const refreshToken = this.tokens?.refreshToken;
    if (!refreshToken) return Promise.resolve(null);
    const cfg = this.config;
    if (!cfg) return Promise.resolve(null);

    const run = (async (): Promise<Tokens | null> => {
      try {
        const tokens = await this.tokenRequest(
          new URLSearchParams({
            grant_type: 'refresh_token',
            refresh_token: refreshToken,
            client_id: cfg.clientId,
            scope: SCOPE,
          }),
        );
        // A provider that rotates returns a new refresh token; one that does
        // not returns none, and the old one stays valid.
        this.adopt({
          accessToken: tokens.accessToken,
          refreshToken: tokens.refreshToken ?? refreshToken,
          expiresAt: tokens.expiresAt,
        });
        return this.tokens;
      } catch {
        return null;
      } finally {
        this.renewInflight = null;
      }
    })();
    this.renewInflight = run;
    return run;
  }

  // --- the API seam ---------------------------------------------------------

  /**
   * Fetch with `Authorization: Bearer <JWT>` attached — the ONE way the portal
   * talks to `/api/v1` (§4.8).
   *
   * 401 ⇒ refresh, retry once, and if that still fails run the redirect flow.
   * 403 ⇒ a valid token without the operator role: flip to the missing-role
   * page and return the response. Never a login loop — logging in again would
   * produce the same token with the same missing role.
   */
  async authorizedFetch(path: string, init: RequestInit = {}): Promise<Response> {
    const token = this.tokens?.accessToken;
    if (!token) {
      await this.beginLogin();
      throw new AuthRedirect();
    }
    let res = await this.deps.fetch(path, withBearer(init, token));
    if (res.status === 403) {
      this.setState('forbidden');
      return res;
    }
    if (res.status !== 401) return res;

    const renewed = await this.tryRenew();
    if (renewed) {
      // `init.body` is always a string here (every mutating call serializes
      // JSON), so replaying it is safe; a stream body would not be.
      res = await this.deps.fetch(path, withBearer(init, renewed.accessToken));
      if (res.status === 403) {
        this.setState('forbidden');
        return res;
      }
      if (res.status !== 401) return res;
    }
    this.abandon();
    await this.beginLogin();
    throw new AuthRedirect();
  }

  /**
   * Drop the tokens and, when the provider offers one, bounce through its
   * end-session endpoint. Logout is client-side by definition here: there is no
   * server-side session and no cookie to clear (D17).
   */
  async logout(): Promise<void> {
    this.tokens = null;
    if (this.renewTimer !== null) this.deps.clearTimer(this.renewTimer);
    this.renewTimer = null;
    this.clearFlow();
    this.setState('idle');
    const meta = this.meta;
    if (meta?.end_session_endpoint) {
      const here = new URL(this.deps.currentUrl());
      const params = new URLSearchParams({
        post_logout_redirect_uri: here.origin + here.pathname,
      });
      if (this.config?.clientId) params.set('client_id', this.config.clientId);
      this.deps.redirect(`${meta.end_session_endpoint}?${params.toString()}`);
    }
  }
}

function withBearer(init: RequestInit, token: string): RequestInit {
  const headers = new Headers(init.headers);
  headers.set('Authorization', `Bearer ${token}`);
  return { ...init, headers, cache: 'no-store' };
}

/**
 * The `nonce` claim of an id_token, read WITHOUT verifying its signature.
 *
 * That is deliberate and is not a security decision: nothing in the browser
 * trusts this token, the backend validates the access token against the
 * issuer's JWKS (§4.8), and this comparison only confirms that the response we
 * are holding answers the request we sent. Returns undefined when the token is
 * unreadable or carries no nonce, which the caller treats as "nothing to
 * check".
 */
function idTokenNonce(idToken: string): string | undefined {
  const part = idToken.split('.')[1];
  if (!part) return undefined;
  try {
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/');
    const json = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, '='));
    return (JSON.parse(json) as { nonce?: string }).nonce;
  } catch {
    return undefined;
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
