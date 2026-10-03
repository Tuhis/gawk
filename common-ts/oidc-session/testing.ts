// The fake-IdP harness for `AuthSession`, shared by this package's own tests
// and by each consumer's tests of its API-client binding (docs/55 D3).
//
// TEST-ONLY. It imports `vitest`, which this package does not depend on: it
// resolves from the consumer whose vitest run imports it, exactly as the
// package's own `session.test.ts` does. Never import it from app code — the
// `./testing` export exists for test files, and nothing in a bundle reaches it.
//
// The harness runs a whole redirect flow against a fake IdP, and it models the
// one thing that makes this flow awkward: **the page reloads in the middle of
// it**. `login()` therefore builds a session, redirects, and then throws that
// session away and builds a second one for the callback — exactly what the
// browser does. Anything the flow needs across that boundary has to be in
// storage, and everything that must NOT be there is asserted afterwards.

import { vi } from 'vitest';

import { AuthSession, flowStorageKey } from './session.ts';
import type { SessionDeps, SessionOptions } from './session.ts';

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
export const FAKE_ONLY: NonNullable<Parameters<typeof vi.useFakeTimers>[0]>['toFake'] = [
  'setTimeout',
  'clearTimeout',
  'Date',
];

/**
 * The options every harness session is built with unless a test passes its
 * own. The bootstrap path is gawk-admin's, because the bootstrap test asserts
 * on that literal; the key prefix is deliberately nobody's.
 */
export const TEST_OPTIONS: SessionOptions = {
  configPath: 'auth/config',
  storageKeyPrefix: 'gawk-oidc-session-test',
};

/** The one key a TEST_OPTIONS session writes. */
export const FLOW_STORAGE_KEY = flowStorageKey(TEST_OPTIONS.storageKeyPrefix);

export const ISSUER ='https://idp.example/realms/gawk';
export const AUTHORIZE = `${ISSUER}/protocol/openid-connect/auth`;
export const TOKEN_URL = `${ISSUER}/protocol/openid-connect/token`;
export const END_SESSION = `${ISSUER}/protocol/openid-connect/logout`;
export const PORTAL = 'https://admin.example/';
export const CLIENT_ID = 'gawk-portal';

export function json(body: unknown, status = 200): Response {
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

export interface Recorded {
  url: string;
  init: RequestInit;
}

/**
 * A queued token-endpoint behaviour. It may return a PROMISE, which is what
 * lets a test pin a token request in flight and decide when — and whether —
 * it lands; the logout races in session.test.ts are exactly that shape.
 */
export type Responder = (init: RequestInit) => Response | Promise<Response>;

export function harness(
  apiHandler?: (url: string, init: RequestInit) => Response,
  options: SessionOptions = TEST_OPTIONS,
) {
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
      if (url === options.configPath) {
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
    newSession: () => new AuthSession(options, deps),
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

export type Harness = ReturnType<typeof harness>;

export function flowRecord(): { state: string; verifier: string; nonce: string; returnTo: string } {
  const raw = window.sessionStorage.getItem(FLOW_STORAGE_KEY);
  if (!raw) throw new Error('no flow record was stored');
  return JSON.parse(raw) as ReturnType<typeof flowRecord>;
}

/** Run a full redirect flow, page reload and all. Returns the signed-in session. */
export async function login(h: Harness): Promise<AuthSession> {
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

export async function until(what: string, predicate: () => boolean, turns = 500): Promise<void> {
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
export async function settle(turns = 50): Promise<void> {
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
export function pinnedToken(): {
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
export function dump(store: Storage): string {
  const parts: string[] = [];
  for (let i = 0; i < store.length; i++) {
    const key = store.key(i);
    if (key === null) continue;
    parts.push(key, store.getItem(key) ?? '');
  }
  return parts.join('\u0000');
}
