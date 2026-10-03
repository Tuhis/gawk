// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AuthRedirect } from '@gawk/oidc-session';
import { AUTHORIZE, ISSUER, harness, json, login } from '@gawk/oidc-session/testing';

import { ApiClient } from '../api/client.ts';

// AP6's auth criteria for the portal's binding of the shared session
// (docs/42 §4.8, §9; docs/55 D3).
//
// The session itself — the redirect flow, token storage, renewal and logout —
// is `@gawk/oidc-session` and is tested where it lives, in
// common-ts/oidc-session/session.test.ts. What stays here is the half that
// needs THIS app's `ApiClient`: that every route it can reach goes through
// `authorizedFetch`, and that 401 and 403 reach the portal as designed. The
// fake IdP is the package's own harness, so the two files test one flow.

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.useRealTimers();
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
