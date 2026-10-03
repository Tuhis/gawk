// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AuthRedirect, flowStorageKey } from '@gawk/oidc-session';
import {
  AUTHORIZE,
  END_SESSION,
  FAKE_ONLY,
  ISSUER,
  PORTAL,
  TOKEN_URL,
  dump,
  harness,
  json,
  until,
  type Harness,
} from '@gawk/oidc-session/testing';

import * as api from '../api/client.ts';
import { IdpBanner, SignedIn } from '../components/Chrome.tsx';
import { useLiveStore } from '../state/liveStore.ts';
import { bootstrapAuth, IdpUnavailable, SESSION_OPTIONS, useAuthStore } from './auth.ts';
import { AuthGate } from './AuthGate.tsx';

// docs/55 TO3: the dashboard's binding of the shared OIDC session.
//
// The session itself — the redirect flow, in-memory tokens, renewal, logout
// races — is `@gawk/oidc-session` and is tested where it lives. What is tested
// here is the half that is this SPA's own: the mode probe, that every read the
// client and the live feed can make goes through `authorizedFetch`, and that
// 401, 403, `idp_unavailable` and `event: expired` reach the page as designed.
// The fake IdP is the package's harness, so both files test one flow.

const FLOW_KEY = flowStorageKey(SESSION_OPTIONS.storageKeyPrefix);
const snapshot = { atMs: 1_700_000_000_000, live: [], ended: [] };

/** Every request that went to the read API rather than to the IdP or the probe. */
function readCalls(h: Harness) {
  return h.calls.filter((c) => !c.url.startsWith(ISSUER) && c.url !== SESSION_OPTIONS.configPath);
}

/** A whole sign-in, page reload and all — `login()` with this SPA's options. */
async function signIn(h: Harness) {
  const boot = () => bootstrapAuth({ fetch: h.deps.fetch, sessionDeps: h.deps });
  await boot();
  expect(h.redirects[0]?.startsWith(AUTHORIZE)).toBe(true);
  const record = JSON.parse(window.sessionStorage.getItem(FLOW_KEY) ?? 'null') as {
    state: string;
    nonce: string;
  };
  h.setNonce(record.nonce);
  h.setUrl(`${PORTAL}?code=auth-code&state=${encodeURIComponent(record.state)}`);
  await boot();
  const session = useAuthStore.getState().session!;
  expect(session.getState().status).toBe('authenticated');
  return session;
}

/** A server-sent event stream the test writes to. */
function sse() {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start: (c) => {
      controller = c;
    },
  });
  const enc = new TextEncoder();
  return {
    response: new Response(body, { headers: { 'content-type': 'text/event-stream' } }),
    write: (text: string) => controller.enqueue(enc.encode(text)),
    end: () => controller.close(),
  };
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  useAuthStore.setState({ mode: 'pending', session: null, probeError: null, idpUnavailable: null });
  useLiveStore.setState({
    snapshot: null,
    error: null,
    lastOkAt: null,
    mode: 'connecting',
    paused: false,
    pausedAtMs: null,
    gapMs: null,
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the mode probe (docs/55 D5)', () => {
  it('404 is none/basic mode: no session, and every request is a plain fetch', async () => {
    const seen: Array<{ url: string; init?: RequestInit }> = [];
    const fetchStub = vi.fn(async (url: string, init?: RequestInit) => {
      seen.push({ url, init });
      if (url === 'auth/config') return new Response('404 page not found', { status: 404 });
      return new Response(JSON.stringify(snapshot), { status: 200 });
    });
    vi.stubGlobal('fetch', fetchStub);

    await bootstrapAuth();
    expect(useAuthStore.getState().mode).toBe('none');
    expect(useAuthStore.getState().session).toBeNull();

    await api.fetchLive();
    await api.fetchMeta();
    await api.resolveCode('ABC123');
    const reads = seen.filter((c) => c.url !== 'auth/config');
    expect(reads.map((c) => c.url)).toEqual(['live', 'v1/meta', 'v1/resolve']);
    for (const c of reads) {
      expect(new Headers(c.init?.headers).get('Authorization')).toBeNull();
    }
    // Nothing was written anywhere: there is no flow to remember.
    expect(window.sessionStorage.length).toBe(0);
    expect(window.localStorage.length).toBe(0);
  });

  it('the dashboard renders straight through the gate in none mode', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 404 })));
    await bootstrapAuth();
    render(
      <AuthGate>
        <p>the dashboard</p>
      </AuthGate>,
    );
    expect(screen.getByText('the dashboard')).toBeTruthy();
    // And the chrome grows no sign-out control.
    const { container } = render(<SignedIn />);
    expect(container.innerHTML).toBe('');
  });

  it('a read listener that does not route auth/config at all (the SPA fallback) is none mode', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('<!doctype html><title>gawk telemetry</title>', {
            status: 200,
            headers: { 'content-type': 'text/html; charset=utf-8' },
          }),
      ),
    );
    await bootstrapAuth();
    expect(useAuthStore.getState().mode).toBe('none');
  });

  it('any other answer leaves the mode unknown and says so, rather than guessing', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 502 })));
    await bootstrapAuth();
    expect(useAuthStore.getState().mode).toBe('failed');
    render(
      <AuthGate>
        <p>the dashboard</p>
      </AuthGate>,
    );
    expect(screen.queryByText('the dashboard')).toBeNull();
    expect(screen.getByText(/HTTP 502/)).toBeTruthy();

    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('Failed to fetch');
      }),
    );
    await act(() => bootstrapAuth());
    expect(useAuthStore.getState().probeError).toBe('Failed to fetch');
  });

  it('200 starts the code+PKCE flow, with the redirect URI taken from the page', async () => {
    const h = harness(undefined, SESSION_OPTIONS);
    h.setUrl('https://telemetry.example/sub/path/?x=1#/history');
    await bootstrapAuth({ fetch: h.deps.fetch, sessionDeps: h.deps });
    expect(useAuthStore.getState().mode).toBe('oidc');
    const url = new URL(h.redirects[0]);
    expect(url.origin + url.pathname).toBe(AUTHORIZE);
    // origin + pathname: no query, no fragment — what the IdP has registered,
    // byte for byte, on the Ingress host and on a port-forward alike.
    expect(url.searchParams.get('redirect_uri')).toBe('https://telemetry.example/sub/path/');
    // The flow record is this SPA's, under this SPA's key.
    expect(JSON.parse(window.sessionStorage.getItem(FLOW_KEY)!).returnTo).toBe('#/history');
  });

  it('the dashboard does not mount until a token is held', async () => {
    const h = harness(undefined, SESSION_OPTIONS);
    await bootstrapAuth({ fetch: h.deps.fetch, sessionDeps: h.deps });
    render(
      <AuthGate>
        <p>the dashboard</p>
      </AuthGate>,
    );
    expect(screen.queryByText('the dashboard')).toBeNull();
    expect(screen.getByText('Signing in…')).toBeTruthy();
  });
});

describe('every read carries the bearer token (docs/55 D4)', () => {
  function handler() {
    return harness((url, init) => {
      if (url === 'live/stream') return sse().response;
      if (url === 'v1/resolve') return json({ broadcastKey: 'bk', roomKey: 'rk' });
      if (init.method === 'DELETE') return new Response(null, { status: 204 });
      if (url.startsWith('v1/fields') || url.startsWith('v1/rules') || url.startsWith('v1/annotations?'))
        return json([]);
      return json({});
    }, SESSION_OPTIONS);
  }

  it('attaches Authorization to every call the client can make, the resolveCode POST included', async () => {
    const h = handler();
    await signIn(h);
    const ctrl = new AbortController();

    const calls: Array<() => Promise<unknown>> = [
      () => api.fetchLive(),
      () => api.fetchMeta(),
      () => api.fetchMe(),
      () => api.fetchSession('s1', { fields: ['fps'], points: 10 }),
      () => api.fetchDiagnose('s1'),
      () => api.fetchDiagnoseTrace('s1'),
      () => api.fetchCompare('s1', 1),
      () => api.fetchDips('s1', 1),
      () => api.fetchHistorySessions({ role: 'viewer' }),
      () => api.fetchHistoryBroadcasts({}),
      () => api.fetchBroadcast('bk'),
      () => api.fetchBroadcastDiagnose('bk'),
      () => api.fetchFields(),
      () => api.fetchRules(),
      () => api.fetchFleetTimeline({}),
      () => api.fetchTrends({ metric: 'fps' }),
      () => api.fetchCohorts({ metric: 'fps' }),
      () => api.fetchAnnotations({ sessionId: 's1' }),
      () => api.createAnnotation({ text: 'x', atMs: 1 } as never),
      () => api.deleteAnnotation('a1'),
      () => api.fetchQueryStatus(),
      () => api.runQuery('select 1'),
      () => api.resolveCode('ABC123'),
      () => api.resolveRoom('room'),
      () => api.probeResolve(),
      () => api.openLiveStream(ctrl.signal),
    ];
    for (const call of calls) await call();
    ctrl.abort();

    const reads = readCalls(h);
    expect(reads).toHaveLength(calls.length);
    for (const call of reads) {
      expect(h.bearerOf(call), `${call.init.method ?? 'GET'} ${call.url}`).toBe('Bearer access-1');
    }
    const resolve = reads.find((c) => c.url === 'v1/resolve' && String(c.init.body).includes('ABC123'));
    expect(resolve?.init.method).toBe('POST');
  });

  it('never sends the bearer token to the identity provider', async () => {
    const h = handler();
    await signIn(h);
    await api.fetchLive();
    for (const call of h.calls.filter((c) => c.url.startsWith(ISSUER))) {
      expect(h.bearerOf(call)).toBeNull();
    }
  });

  it('keeps nothing auth-shaped in localStorage, and nothing but the transient flow in sessionStorage', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = handler();
    const session = await signIn(h);
    await api.fetchLive();
    // Through a renewal too: rotation must not find somewhere to write either.
    await vi.advanceTimersByTimeAsync(250_000);
    await until('the silent renewal', () => session.accessToken() === 'access-2');
    await api.fetchLive();

    expect(window.localStorage.length).toBe(0);
    // The PKCE record is deleted the instant the code is exchanged.
    expect(window.sessionStorage.getItem(FLOW_KEY)).toBeNull();
    for (const store of [window.localStorage, window.sessionStorage]) {
      expect(dump(store)).not.toMatch(/access-|refresh-|auth-code/);
    }
  });
});

describe('renewal and the redirect fallback (docs/55 G2)', () => {
  it('renews silently before expiry, and the next read carries the new token', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness(() => json(snapshot), SESSION_OPTIONS);
    const session = await signIn(h);

    // A 300 s token renews a minute early. Just before that, nothing.
    await vi.advanceTimersByTimeAsync(239_000);
    expect(h.tokenCalls()).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(2_000);
    await until('the silent renewal', () => session.accessToken() === 'access-2');
    expect(h.bodyOf(h.tokenCalls()[1]).get('grant_type')).toBe('refresh_token');
    expect(h.redirects).toHaveLength(1);

    await api.fetchLive();
    expect(h.bearerOf(readCalls(h).at(-1)!)).toBe('Bearer access-2');
  });

  it('a failed renewal runs the redirect flow', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const h = harness(() => json(snapshot), SESSION_OPTIONS);
    const session = await signIn(h);
    h.tokenPlan.push(() => json({ error: 'invalid_grant' }, 400));

    await vi.advanceTimersByTimeAsync(241_000);
    await until('the redirect', () => h.redirects.length === 2);
    expect(h.redirects[1].startsWith(AUTHORIZE)).toBe(true);
    expect(session.accessToken()).toBeNull();
  });

  it('401 refreshes and retries the same read', async () => {
    const h = harness((url, init) => {
      if (url !== 'v1/meta') return json({});
      return new Headers(init.headers).get('Authorization') === 'Bearer access-2'
        ? json({ retentionDays: 7 })
        : new Response(null, { status: 401 });
    }, SESSION_OPTIONS);
    await signIn(h);
    const meta = await api.fetchMeta();
    expect(meta.retentionDays).toBe(7);
    expect(h.tokenCalls()).toHaveLength(2);
    expect(h.redirects).toHaveLength(1);
  });

  it('401 that survives the refresh runs the redirect flow', async () => {
    const h = harness(() => new Response(null, { status: 401 }), SESSION_OPTIONS);
    await signIn(h);
    await expect(api.fetchMeta()).rejects.toBeInstanceOf(AuthRedirect);
    expect(h.bodyOf(h.tokenCalls()[1]).get('grant_type')).toBe('refresh_token');
    expect(h.redirects).toHaveLength(2);
    expect(h.redirects[1].startsWith(AUTHORIZE)).toBe(true);
  });
});

describe('403 and idp_unavailable are not login problems', () => {
  it('403 renders the missing-role page, naming the identity, instead of looping', async () => {
    const h = harness((url) => {
      if (url === 'v1/me') return json({ subject: 'u-1', email: 'op@example.com', roles: ['viewer'] });
      return json({ error: { code: 'forbidden', message: 'telemetry-reader role required' } }, 403);
    }, SESSION_OPTIONS);
    const session = await signIn(h);
    render(
      <AuthGate>
        <p>the dashboard</p>
      </AuthGate>,
    );
    expect(screen.getByText('the dashboard')).toBeTruthy();

    await act(async () => {
      await expect(api.fetchMeta()).rejects.toThrow();
    });
    expect(session.getState().status).toBe('forbidden');
    expect(screen.queryByText('the dashboard')).toBeNull();
    expect(screen.getByText('Not a telemetry reader')).toBeTruthy();
    expect(await screen.findByText('op@example.com')).toBeTruthy();
    expect(screen.getByText(/viewer/)).toBeTruthy();
    // The token is fine; signing in again would mint the same one.
    expect(h.redirects).toHaveLength(1);
    expect(h.tokenCalls()).toHaveLength(1);

    fireEvent.click(screen.getByText('Sign out'));
    expect(h.redirects.at(-1)!.startsWith(END_SESSION)).toBe(true);
  });

  it('401 idp_unavailable shows a banner and neither refreshes nor redirects', async () => {
    let down = true;
    const h = harness(() =>
      down
        ? json({ error: { code: 'idp_unavailable', message: 'OIDC discovery has not completed' } }, 401)
        : json(snapshot),
    SESSION_OPTIONS);
    await signIn(h);
    render(<IdpBanner />);

    await act(async () => {
      await expect(api.fetchLive()).rejects.toBeInstanceOf(IdpUnavailable);
    });
    expect(h.tokenCalls()).toHaveLength(1);
    expect(h.redirects).toHaveLength(1);
    expect(screen.getByRole('alert').textContent).toMatch(/cannot reach its identity provider/);
    expect(screen.getByRole('alert').textContent).toMatch(/discovery has not completed/);

    // Recovers on its own: the next read that gets through clears it.
    down = false;
    await act(async () => {
      await api.fetchLive();
    });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('the chrome shows who is signed in, and signs out', async () => {
    const h = harness(
      (url) => (url === 'v1/me' ? json({ subject: 'u-1', email: 'op@example.com', roles: [] }) : json({})),
      SESSION_OPTIONS,
    );
    const session = await signIn(h);
    render(<SignedIn />);
    expect(await screen.findByText('op@example.com')).toBeTruthy();
    fireEvent.click(screen.getByText('sign out'));
    expect(session.accessToken()).toBeNull();
    expect(h.redirects.at(-1)!.startsWith(END_SESSION)).toBe(true);
  });
});

describe('the live stream in OIDC mode (docs/55 D4)', () => {
  it('event: expired reconnects at once, with the renewed token', async () => {
    vi.useFakeTimers({ toFake: FAKE_ONLY });
    const streams: Array<ReturnType<typeof sse> & { bearer: string | null }> = [];
    const h = harness((url, init) => {
      if (url !== 'live/stream') return json(snapshot);
      const s = { ...sse(), bearer: new Headers(init.headers).get('Authorization') };
      streams.push(s);
      return s.response;
    }, SESSION_OPTIONS);
    const session = await signIn(h);

    const stop = useLiveStore.getState().start();
    await until('the stream', () => useLiveStore.getState().mode === 'stream');
    expect(streams[0].bearer).toBe('Bearer access-1');
    streams[0].write(`event: snapshot\ndata: ${JSON.stringify(snapshot)}\n\n`);
    await until('the snapshot', () => useLiveStore.getState().snapshot !== null);

    // The session renews ahead of expiry; the open stream still runs on the
    // old token until the server caps it at that token's exp.
    await vi.advanceTimersByTimeAsync(241_000);
    await until('the silent renewal', () => session.accessToken() === 'access-2');
    streams[0].write('event: expired\ndata: {}\n\n');
    streams[0].end();

    await until('the reconnect', () => streams.length === 2);
    expect(streams[1].bearer).toBe('Bearer access-2');
    await until('the stream again', () => useLiveStore.getState().mode === 'stream');
    // No poll in between: an expiry is not a failure.
    expect(readCalls(h).filter((c) => c.url === 'live')).toHaveLength(0);
    stop();
  });

  it('the poll takes over, with the bearer, when the stream fails', async () => {
    const h = harness((url) => {
      if (url === 'live/stream') return new Response('upstream buffered it', { status: 502 });
      return json(snapshot);
    }, SESSION_OPTIONS);
    await signIn(h);

    const stop = useLiveStore.getState().start();
    await until('the poll', () => useLiveStore.getState().snapshot !== null);
    expect(useLiveStore.getState().mode).toBe('poll');
    const polls = readCalls(h).filter((c) => c.url === 'live');
    expect(polls.length).toBeGreaterThan(0);
    for (const p of polls) expect(h.bearerOf(p)).toBe('Bearer access-1');
    stop();
  });

  it('a stream request that bounces to the IdP schedules nothing behind it', async () => {
    const h = harness(() => new Response(null, { status: 401 }), SESSION_OPTIONS);
    await signIn(h);
    const stop = useLiveStore.getState().start();
    await until('the redirect', () => h.redirects.length === 2);
    // The page is leaving: no poll is started against a dead credential.
    expect(useLiveStore.getState().mode).toBe('connecting');
    expect(h.calls.filter((c) => c.url === TOKEN_URL)).toHaveLength(2);
    stop();
  });
});
