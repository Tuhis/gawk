// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { codeChallengeS256 } from './pkce.ts';
import { AuthRedirect, AuthSession, flowStorageKey } from './session.ts';
import {
  AUTHORIZE,
  END_SESSION,
  FAKE_ONLY,
  FLOW_STORAGE_KEY,
  PORTAL,
  CLIENT_ID,
  dump,
  flowRecord,
  harness,
  json,
  login,
  pinnedToken,
  settle,
  until,
} from './testing.ts';

// AP6's auth criteria, as tests (docs/42 §4.8, §9), for the session every gawk
// SPA shares (docs/55 D3). The fake IdP and the sync helpers are in
// `testing.ts`, which each consumer's tests import too.
//
// What needs a consumer's API client — every route carrying the bearer token,
// 401 versus 403 as the views see them — is tested by that consumer against
// this harness (gawk-admin: ui/src/auth/session.test.ts).

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
    const everything = h.calls.map((c) => h.bearerOf(c) ?? '').join('\u0000');
    expect(everything).not.toContain('access-2');
  });

  // Regression (PR #280 review, second pass). The THIRD sibling of the same
  // bug, and the one with the widest window: `authorizedFetch`'s 401 tail.
  //
  // A polling view's fetch 401s on a token that has just expired, its refresh
  // goes in flight, and the operator clicks Sign out. The refresh is correctly
  // refused as stale — and the fallthrough then walked to the IdP anyway. The
  // authorize request carries no `prompt`, so an IdP holding a live SSO
  // session answers it immediately and the operator is signed back in without
  // ever seeing a login screen. In-flight requests at sign-out time are
  // routine: the fleet view polls every five seconds.
  it('does not sign back in when a 401 fallback is overtaken by Sign out', async () => {
    const h = harness((url) =>
      url.startsWith('api/v1/') ? new Response(null, { status: 401 }) : json({}),
    );
    const session = await login(h);

    const pinned = pinnedToken();
    h.tokenPlan.push(pinned.responder);
    const inflight = session.authorizedFetch('api/v1/broadcasts');
    await until('the 401 refresh to be in flight', () => h.tokenCalls().length === 2);

    await session.logout();
    expect(h.redirects).toHaveLength(2);
    expect(h.redirects[1].startsWith(END_SESSION)).toBe(true);

    // The IdP answers the refresh it was already handling: valid tokens for a
    // session that has just ended.
    pinned.deliver({
      access_token: 'access-2',
      refresh_token: 'refresh-2',
      token_type: 'Bearer',
      expires_in: 300,
    });

    // Awaiting the call itself is the sync point: it settles only once the
    // whole 401 tail has run, redirect and all.
    await expect(inflight).rejects.toBeInstanceOf(AuthRedirect);

    expect(h.redirects).toHaveLength(2);
    expect(session.accessToken()).toBeNull();
    expect(session.getState().status).toBe('idle');
    // `logout` cleared the flow record and nothing wrote a new one, so no
    // authorization request was started behind the operator's back.
    expect(window.sessionStorage.getItem(FLOW_STORAGE_KEY)).toBeNull();
  });
});

describe('the consumer-specific values are options (docs/55 D3)', () => {
  // Nothing in the package may name a consumer: a second SPA that inherited
  // the first one's bootstrap path or storage key would talk to the wrong
  // endpoint, or share an in-flight PKCE record with it on a shared origin.
  const OTHER = { configPath: 'other/auth/config', storageKeyPrefix: 'gawk-other' };

  it('bootstraps from the configured path and stores under the configured key', async () => {
    const h = harness(undefined, OTHER);
    await h.newSession().start();

    expect(h.calls.filter((c) => c.url === OTHER.configPath)).toHaveLength(1);
    expect(h.calls.filter((c) => c.url === 'auth/config')).toHaveLength(0);
    expect(h.redirects).toHaveLength(1);
    // The one key, and it is this consumer's.
    expect(window.sessionStorage.length).toBe(1);
    expect(window.sessionStorage.getItem(flowStorageKey(OTHER.storageKeyPrefix))).not.toBeNull();
    expect(window.sessionStorage.getItem(FLOW_STORAGE_KEY)).toBeNull();
  });

  it('names the configured path when the bootstrap fails', async () => {
    const session = new AuthSession(OTHER, {
      fetch: (async () => new Response(null, { status: 503 })) as typeof globalThis.fetch,
      currentUrl: () => PORTAL,
      storage: window.sessionStorage,
    });
    await session.start();
    expect(session.getState()).toEqual({
      status: 'error',
      message: 'GET /other/auth/config failed: HTTP 503',
    });
  });
});
