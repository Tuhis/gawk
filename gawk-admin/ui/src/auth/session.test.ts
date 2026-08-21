// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiClient } from '../api/client.ts';
import { AuthRedirect, AuthSession, FLOW_STORAGE_KEY } from './session.ts';
import type { SessionDeps } from './session.ts';
import { codeChallengeS256 } from './pkce.ts';

// AP6's auth criteria, as tests (docs/42 §4.8, §9).
//
// The harness runs a whole redirect flow against a fake IdP, and it models the
// one thing that makes this flow awkward: **the page reloads in the middle of
// it**. `login()` therefore builds a session, redirects, and then throws that
// session away and builds a second one for the callback — exactly what the
// browser does. Anything the flow needs across that boundary has to be in
// storage, and everything that must NOT be there is asserted afterwards.

/**
 * Fake exactly the three clocks `AuthSession` uses, and nothing else.
 *
 * Vitest's default `toFake` also replaces `setImmediate`/`clearImmediate`. That
 * matters here because these tests await real `Response.json()` calls *before*
 * any timer is advanced, and Node's body-stream machinery can ride on
 * `setImmediate`. Faking it would leave such a read waiting for a tick that
 * only `advanceTimersByTime` can deliver — a hang that would depend on how the
 * body happened to be chunked, i.e. exactly the kind of load-sensitive
 * flakiness this narrowing removes by construction.
 */
const FAKE_ONLY: NonNullable<Parameters<typeof vi.useFakeTimers>[0]>['toFake'] = [
  'setTimeout',
  'clearTimeout',
  'Date',
];

const ISSUER = 'https://idp.example/realms/gawk';
const AUTHORIZE = `${ISSUER}/protocol/openid-connect/auth`;
const TOKEN_URL = `${ISSUER}/protocol/openid-connect/token`;
const END_SESSION = `${ISSUER}/protocol/openid-connect/logout`;
const PORTAL = 'https://admin.example/';
const CLIENT_ID = 'gawk-portal';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function b64url(s: string): string {
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** An unsigned id_token. Nothing in the browser verifies one (see session.ts). */
function idToken(nonce: string): string {
  return `${b64url('{"alg":"none"}')}.${b64url(JSON.stringify({ nonce }))}.`;
}

interface Recorded {
  url: string;
  init: RequestInit;
}

/**
 * A queued token-endpoint behaviour. It may return a PROMISE, which is what
 * lets a test pin a token request in flight and decide when — and whether —
 * it lands; the logout race below is exactly that shape.
 */
type Responder = (init: RequestInit) => Response | Promise<Response>;

function harness(apiHandler?: (url: string, init: RequestInit) => Response) {
  const calls: Recorded[] = [];
  const redirects: string[] = [];
  let currentUrl = PORTAL;
  let issued = 0;
  let nonce = '';
  /** Queued token-endpoint behaviours; the default issues a fresh pair. */
  const tokenPlan: Responder[] = [];

  const defaultToken: Responder = () => {
    issued++;
    return json({
      access_token: `access-${issued}`,
      refresh_token: `refresh-${issued}`,
      id_token: idToken(nonce),
      token_type: 'Bearer',
      expires_in: 300,
    });
  };

  const fetchImpl = vi.fn(
    async (input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> => {
      const url = String(input);
      calls.push({ url, init });
      if (url === 'auth/config') {
        return json({ issuer: ISSUER, clientId: CLIENT_ID, audience: 'gawk-admin-api' });
      }
      if (url === `${ISSUER}/.well-known/openid-configuration`) {
        return json({
          authorization_endpoint: AUTHORIZE,
          token_endpoint: TOKEN_URL,
          end_session_endpoint: END_SESSION,
        });
      }
      if (url === TOKEN_URL) return (tokenPlan.shift() ?? defaultToken)(init);
      return apiHandler ? apiHandler(url, init) : json({});
    },
  );

  const deps: Partial<SessionDeps> = {
    fetch: fetchImpl as unknown as typeof globalThis.fetch,
    redirect: (url) => {
      redirects.push(url);
    },
    storage: window.sessionStorage,
    currentUrl: () => currentUrl,
    replaceUrl: (url) => {
      currentUrl = url;
    },
  };

  return {
    calls,
    redirects,
    tokenPlan,
    deps,
    newSession: () => new AuthSession(deps),
    setUrl: (url: string) => {
      currentUrl = url;
    },
    setNonce: (n: string) => {
      nonce = n;
    },
    tokenCalls: () => calls.filter((c) => c.url === TOKEN_URL),
    apiCalls: () => calls.filter((c) => c.url.startsWith('api/v1/')),
    bodyOf: (c: Recorded) => new URLSearchParams(String(c.init.body ?? '')),
    bearerOf: (c: Recorded) => new Headers(c.init.headers).get('Authorization'),
  };
}

type Harness = ReturnType<typeof harness>;

function flowRecord(): { state: string; verifier: string; nonce: string; returnTo: string } {
  const raw = window.sessionStorage.getItem(FLOW_STORAGE_KEY);
  if (!raw) throw new Error('no flow record was stored');
  return JSON.parse(raw) as ReturnType<typeof flowRecord>;
}

/** Run a full redirect flow, page reload and all. Returns the signed-in session. */
async function login(h: Harness): Promise<AuthSession> {
  const first = h.newSession();
  await first.start();
  const record = flowRecord();
  h.setNonce(record.nonce);
  h.setUrl(`${PORTAL}?code=auth-code&state=${encodeURIComponent(record.state)}`);
  // The browser has navigated away and back: a brand-new session object, with
  // nothing in memory.
  const second = h.newSession();
  await second.start();
  return second;
}

/**
 * Wait for an un-awaited async chain to reach an observable state, one real
 * event-loop turn at a time.
 *
 * The scheduled renewal is fired from a timer callback as `void
 * this.silentRenew()` — by design, since nothing in a browser awaits a
 * background refresh. So no test can await it either, and
 * `advanceTimersByTimeAsync` is NOT a sufficient sync point: it runs timer
 * callbacks and drains microtasks, but the renewal's tail calls
 * `codeChallengeS256`, whose `crypto.subtle.digest` resolves from Node's
 * libuv threadpool. A threadpool completion needs a real event-loop turn.
 * Under no load it usually lands inside the flush; under a full parallel `npm
 * test` it often does not — which is precisely the load-sensitive,
 * one-test-in-a-full-run flake this replaces.
 *
 * This is waiting for an operation to finish, not retrying a shaky assertion:
 * it yields turns until the state is observable and then the assertions after
 * it are exact, or it fails loudly having never seen it.
 *
 * The turn is driven by `realSetTimeout`, captured at module load — before any
 * test body can install fake timers — so this loop keeps running while the
 * clock the session sees is frozen. A plain `await` would not do: microtasks
 * never yield to the event loop, which is the whole problem.
 */
const realSetTimeout = globalThis.setTimeout;

async function until(what: string, predicate: () => boolean, turns = 500): Promise<void> {
  for (let i = 0; i < turns; i++) {
    if (predicate()) return;
    await new Promise((resolve) => realSetTimeout(resolve, 0));
  }
  throw new Error(`timed out waiting for ${what}`);
}

/**
 * The other half of `until`: run an un-awaited chain out, so that asserting
 * something did NOT happen means it had every chance to.
 *
 * `until` proves something happens; this backs the opposite claim, which is
 * only worth anything once the chain has actually run. It is used after a
 * precise sync point rather than instead of one — the budget is slack, not the
 * argument.
 */
async function settle(turns = 50): Promise<void> {
  for (let i = 0; i < turns; i++) {
    await new Promise((resolve) => realSetTimeout(resolve, 0));
  }
}

/**
 * A token response the test holds open until it chooses to deliver it, so a
 * logout can land strictly inside the renewal's POST.
 *
 * `consumed` is the sync point that makes the "nothing happened" assertions
 * exact rather than merely patient: `bodyUsed` flips the moment
 * `tokenRequest`'s `res.json()` disturbs the stream, so once it is true the
 * session is inside the very continuation that decides whether to adopt these
 * tokens.
 */
function pinnedToken(): {
  responder: Responder;
  deliver: (body: unknown) => void;
  consumed: () => boolean;
} {
  let resolve!: (res: Response) => void;
  let delivered: Response | null = null;
  const pending = new Promise<Response>((r) => {
    resolve = r;
  });
  return {
    responder: () => pending,
    deliver: (body: unknown) => {
      delivered = json(body);
      resolve(delivered);
    },
    consumed: () => delivered !== null && delivered.bodyUsed,
  };
}

/**
 * Everything currently in a Storage, flattened, for "is anything in here?"
 * checks. Joined on `\u0000` — a byte no key, token or JSON blob can contain —
 * written as an escape rather than as a raw byte, so the file stays text that
 * `git diff` and `grep` will actually show you.
 */
function dump(store: Storage): string {
  const parts: string[] = [];
  for (let i = 0; i < store.length; i++) {
    const key = store.key(i);
    if (key === null) continue;
    parts.push(key, store.getItem(key) ?? '');
  }
  return parts.join('\u0000');
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the redirect flow (§4.8)', () => {
  it('is a public client using authorization code + PKCE, with state and nonce', async () => {
    const h = harness();
    await h.newSession().start();

    expect(h.redirects).toHaveLength(1);
    const url = new URL(h.redirects[0]);
    expect(`${url.origin}${url.pathname}`).toBe(AUTHORIZE);
    expect(url.searchParams.get('response_type')).toBe('code');
    expect(url.searchParams.get('client_id')).toBe(CLIENT_ID);
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
    expect(url.searchParams.get('redirect_uri')).toBe(PORTAL);

    const record = flowRecord();
    expect(url.searchParams.get('state')).toBe(record.state);
    expect(url.searchParams.get('nonce')).toBe(record.nonce);
    // The challenge is really S256(verifier) — not the verifier itself, which
    // is the mistake that turns PKCE into decoration.
    expect(url.searchParams.get('code_challenge')).toBe(await codeChallengeS256(record.verifier));
    expect(url.searchParams.get('code_challenge')).not.toBe(record.verifier);
  });

  it('bootstraps from the unauthenticated /auth/config document', async () => {
    const h = harness();
    await h.newSession().start();
    const bootstrap = h.calls.filter((c) => c.url === 'auth/config');
    expect(bootstrap).toHaveLength(1);
    // Unauthenticated by definition: there is no token yet to send.
    expect(h.bearerOf(bootstrap[0])).toBeNull();
  });

  it('exchanges the code with the verifier and no client secret anywhere', async () => {
    const h = harness();
    const session = await login(h);
    expect(session.accessToken()).toBe('access-1');

    const exchange = h.bodyOf(h.tokenCalls()[0]);
    expect(exchange.get('grant_type')).toBe('authorization_code');
    expect(exchange.get('code')).toBe('auth-code');
    expect(exchange.get('code_verifier')).toBeTruthy();
    expect(exchange.get('client_secret')).toBeNull();

    // Belt and braces: no secret in ANY request this flow made.
    const everything = h.calls
      .map((c) => `${c.url}\u0000${String(c.init.body ?? '')}`)
      .join('\u0000');
    expect(everything).not.toContain('client_secret');
  });

  // Regression, found while auditing the renewal path (AP6 follow-up).
  //
  // `beginLogin` writes the PKCE record, then AWAITS the S256 challenge, then
  // redirects. Two overlapping calls therefore interleave as: write record1,
  // await, write record2 (clobbering record1), await, redirect with state1,
  // redirect with state2. Whichever navigation the browser actually commits,
  // sessionStorage holds only the LAST record — so if the first one wins, the
  // callback fails the state check and the operator lands on "authorization
  // response did not match the request" with nothing they can do about it.
  //
  // Overlap is ordinary, not exotic: a failed silent renewal drops the tokens
  // and calls beginLogin, and the broadcasts view's 5 s poll then finds no
  // token and calls it again while the first is still awaiting discovery.
  it('starts only ONE authorization request when asked twice at once', async () => {
    const h = harness();
    const session = h.newSession();

    await Promise.all([session.beginLogin(), session.beginLogin()]);

    expect(h.redirects).toHaveLength(1);
    // The surviving record must describe the request the browser was sent on.
    const record = flowRecord();
    const sent = new URL(h.redirects[0]);
    expect(sent.searchParams.get('state')).toBe(record.state);
    expect(sent.searchParams.get('nonce')).toBe(record.nonce);
    expect(sent.searchParams.get('code_challenge')).toBe(
      await codeChallengeS256(record.verifier),
    );
  });

  it('can start a fresh authorization request after one has finished', async () => {
    // The single-flight above must not latch: "Try again" on the sign-in error
    // page calls beginLogin again, and it has to work.
    const h = harness();
    const session = h.newSession();
    await session.beginLogin();
    await session.beginLogin();
    expect(h.redirects).toHaveLength(2);
  });

  it('refuses a callback whose state does not match the request', async () => {
    const h = harness();
    const first = h.newSession();
    await first.start();
    h.setUrl(`${PORTAL}?code=auth-code&state=not-the-one`);
    const second = h.newSession();
    await second.start();

    expect(second.getState().status).toBe('error');
    expect(second.accessToken()).toBeNull();
    expect(h.tokenCalls()).toHaveLength(0);
  });
});

describe('tokens are held in memory only (§4.8, AP6)', () => {
  it('puts nothing auth-shaped in localStorage, before or after renewal', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness();
    const session = await login(h);

    // The whole point: localStorage is never touched at all.
    expect(window.localStorage.length).toBe(0);

    await vi.advanceTimersByTimeAsync(241_000);
    await until('the scheduled renewal to complete', () => session.accessToken() === 'access-2');
    expect(session.accessToken()).toBe('access-2');
    expect(window.localStorage.length).toBe(0);

    const everywhere = dump(window.localStorage) + dump(window.sessionStorage);
    for (const secret of ['access-1', 'access-2', 'refresh-1', 'refresh-2']) {
      expect(everywhere).not.toContain(secret);
    }
  });

  it('keeps only the transient PKCE record in sessionStorage, and deletes it on callback', async () => {
    const h = harness();
    const first = h.newSession();
    await first.start();

    // Mid-flight the ONE key exists — the verifier, state and nonce have to
    // survive a full-page navigation, and none of them is a token.
    expect(window.sessionStorage.length).toBe(1);
    const record = flowRecord();
    expect(record.verifier).toBeTruthy();
    expect(window.localStorage.length).toBe(0);

    h.setNonce(record.nonce);
    h.setUrl(`${PORTAL}?code=auth-code&state=${encodeURIComponent(record.state)}`);
    await h.newSession().start();

    // A spent verifier must never be offered twice.
    expect(window.sessionStorage.getItem(FLOW_STORAGE_KEY)).toBeNull();
    expect(window.sessionStorage.length).toBe(0);
  });

  it('loses its tokens on reload, which re-runs the redirect flow', async () => {
    const h = harness();
    await login(h);
    // A brand-new session object is what a reload produces: empty memory.
    const reloaded = h.newSession();
    expect(reloaded.accessToken()).toBeNull();
    await reloaded.start();
    expect(h.redirects.length).toBeGreaterThan(1);
  });
});

describe('every /api/v1 call carries the bearer token (§4.8)', () => {
  it('attaches Authorization to every route the client can reach', async () => {
    const h = harness((url) => {
      if (url.startsWith('api/v1/bans/')) return new Response(null, { status: 204 });
      return json({});
    });
    const session = await login(h);
    const api = new ApiClient(session);

    await api.me();
    await api.broadcasts();
    await api.kill('ABC123', { reason: 'terms', cooldownSeconds: 600 });
    await api.bans('active');
    await api.createBan({
      target: { type: 'broadcastId', value: 'ABC123' },
      expiresAt: null,
      reason: 'terms',
    });
    await api.unban('ban-1');
    await api.events();
    await api.relays();
    await api.webhooks();
    await api.testWebhook('ntfy');

    const apiCalls = h.apiCalls();
    expect(apiCalls.length).toBe(10);
    for (const call of apiCalls) {
      expect(h.bearerOf(call)).toBe('Bearer access-1');
    }
  });

  it('never sends the bearer token to the identity provider', async () => {
    const h = harness();
    const session = await login(h);
    await new ApiClient(session).me();
    for (const call of h.calls.filter((c) => c.url.startsWith(ISSUER))) {
      expect(h.bearerOf(call)).toBeNull();
    }
  });
});

describe('silent renewal by refresh-token rotation (§4.8, AP6)', () => {
  it('renews before the access token expires, and rotates the refresh token', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness();
    const session = await login(h);

    // 300 s token: nothing yet at 239 s.
    await vi.advanceTimersByTimeAsync(239_000);
    expect(h.tokenCalls()).toHaveLength(1);
    expect(session.accessToken()).toBe('access-1');

    await vi.advanceTimersByTimeAsync(2_000);
    await until('the first renewal to complete', () => h.tokenCalls().length === 2);
    const refresh = h.tokenCalls()[1];
    expect(h.bodyOf(refresh).get('grant_type')).toBe('refresh_token');
    expect(h.bodyOf(refresh).get('refresh_token')).toBe('refresh-1');
    expect(session.accessToken()).toBe('access-2');

    // Rotation: the SECOND renewal must present the token the first one
    // returned. Presenting `refresh-1` again against a rotating IdP is a
    // guaranteed failure, and it is the bug this assertion exists for.
    await vi.advanceTimersByTimeAsync(241_000);
    await until('the second renewal to complete', () => h.tokenCalls().length === 3);
    expect(h.bodyOf(h.tokenCalls()[2]).get('refresh_token')).toBe('refresh-2');
    expect(session.accessToken()).toBe('access-3');
  });

  it('falls back to the full redirect flow when renewal fails', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness();
    const session = await login(h);
    expect(h.redirects).toHaveLength(1);

    // The IdP has revoked the session, or the refresh token was already used.
    h.tokenPlan.push(() => json({ error: 'invalid_grant' }, 400));
    await vi.advanceTimersByTimeAsync(241_000);
    await until('the fallback redirect to be issued', () => h.redirects.length === 2);

    expect(h.redirects).toHaveLength(2);
    expect(h.redirects[1].startsWith(AUTHORIZE)).toBe(true);
    expect(session.accessToken()).toBeNull();
  });
});

describe('401 and 403 are answered differently (§4.8, AP6)', () => {
  it('401 refreshes and retries the same request', async () => {
    const h = harness((url, init) => {
      if (!url.startsWith('api/v1/')) return json({});
      const bearer = new Headers(init.headers).get('Authorization');
      return bearer === 'Bearer access-2'
        ? json({ email: 'op@example.com', subject: 's', roles: ['operator'] })
        : new Response(null, { status: 401 });
    });
    const session = await login(h);
    const me = await new ApiClient(session).me();

    expect(me.email).toBe('op@example.com');
    expect(h.bodyOf(h.tokenCalls()[1]).get('grant_type')).toBe('refresh_token');
    // Repaired in place: no redirect was needed.
    expect(h.redirects).toHaveLength(1);
  });

  it('401 that survives the refresh runs the redirect flow', async () => {
    const h = harness((url) =>
      url.startsWith('api/v1/') ? new Response(null, { status: 401 }) : json({}),
    );
    const session = await login(h);

    await expect(new ApiClient(session).me()).rejects.toBeInstanceOf(AuthRedirect);
    expect(h.bodyOf(h.tokenCalls()[1]).get('grant_type')).toBe('refresh_token');
    expect(h.redirects).toHaveLength(2);
    expect(h.redirects[1].startsWith(AUTHORIZE)).toBe(true);
  });

  it('403 renders the missing-role page instead of looping through login', async () => {
    const h = harness((url) =>
      url.startsWith('api/v1/')
        ? json({ error: { code: 'forbidden', message: 'operator role required' } }, 403)
        : json({}),
    );
    const session = await login(h);

    await expect(new ApiClient(session).me()).rejects.toThrow(/operator role required/);
    expect(session.getState().status).toBe('forbidden');
    // The token is fine and the identity is fine; signing in again would
    // produce the same token with the same missing role.
    expect(h.redirects).toHaveLength(1);
    expect(h.tokenCalls()).toHaveLength(1);
    expect(session.accessToken()).toBe('access-1');
  });
});

describe('logout (§4.8)', () => {
  it('drops the tokens and bounces through the end-session endpoint', async () => {
    const h = harness();
    const session = await login(h);
    await session.logout();

    expect(session.accessToken()).toBeNull();
    expect(session.getState().status).toBe('idle');
    expect(h.redirects[1].startsWith(END_SESSION)).toBe(true);
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
  });

  // Regression (PR #280 review). The renewal timer fires on its own schedule,
  // so "Sign out while a silent renew is in flight" is not a contrived
  // interleaving — it is one click landing inside a ~1 s window that recurs
  // every few minutes.
  //
  // `tryRenew`'s closure used to call `adopt` unconditionally on the response.
  // With an IdP whose discovery document carries no `end_session_endpoint`
  // (it is OPTIONAL, so `logout` performs no navigation and the page stays
  // put) the operator would watch the portal sign itself back in — on a shared
  // or incident-response machine, which is the situation Sign out exists for.
  it('does not let a renewal that was already in flight resurrect the session', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness();
    const session = await login(h);

    // Hold the renewal's POST open so the logout lands strictly inside it.
    const pinned = pinnedToken();
    h.tokenPlan.push(pinned.responder);
    await vi.advanceTimersByTimeAsync(241_000);
    await until('the renewal POST to be in flight', () => h.tokenCalls().length === 2);
    expect(session.accessToken()).toBe('access-1');

    await session.logout();
    expect(session.accessToken()).toBeNull();

    // The IdP answers the renewal it was already handling. The tokens are
    // valid; they are simply no longer wanted.
    pinned.deliver({
      access_token: 'access-2',
      refresh_token: 'refresh-2',
      token_type: 'Bearer',
      expires_in: 300,
    });
    await until('the renewal response to be read', () => pinned.consumed());
    await settle();

    expect(session.accessToken()).toBeNull();
    expect(session.getState().status).toBe('idle');
    // Nothing was re-scheduled, so the resurrection cannot arrive one renewal
    // later either.
    expect(vi.getTimerCount()).toBe(0);
    // The initial login and the end-session bounce, and nothing since. The
    // other way back in is `silentRenew`'s "renewal failed ⇒ go to the IdP",
    // which against a live IdP session is an invisible round trip that lands
    // the operator signed back in.
    expect(h.redirects).toHaveLength(2);
    // And the seam every view goes through agrees: there is no session here.
    await expect(session.authorizedFetch('api/v1/me')).rejects.toBeInstanceOf(AuthRedirect);
    // The refused token never reached a request.
    const everything = h.calls.map((c) => h.bearerOf(c) ?? '').join(' ');
    expect(everything).not.toContain('access-2');
  });
});
