// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { ApiClient, ApiError } from './client.ts';
import { json, stubSession } from '../testing/harness.tsx';

// The wire contract `internal/api` (AP4) actually serves, asserted rather than
// assumed. An earlier draft of the client accepted either a bare array or a
// keyed envelope, because docs/42 §4.7 named the rows without naming the
// wrapper. There is now one authority, so there is one shape — and these tests
// exist so that shape cannot drift back into a shrug.

function client(handler: (path: string, init: RequestInit) => Response) {
  const session = stubSession(handler);
  return { api: new ApiClient(session), session };
}

describe('list routes are KEYED envelopes', () => {
  const cases: { name: string; path: string; key: string; call: (a: ApiClient) => Promise<unknown[]> }[] =
    [
      { name: 'broadcasts', path: 'api/v1/broadcasts', key: 'broadcasts', call: (a) => a.broadcasts() },
      { name: 'bans', path: 'api/v1/bans?state=active', key: 'bans', call: (a) => a.bans('active') },
      { name: 'relays', path: 'api/v1/relays', key: 'relays', call: (a) => a.relays() },
      { name: 'webhooks', path: 'api/v1/webhooks', key: 'webhooks', call: (a) => a.webhooks() },
    ];

  for (const c of cases) {
    it(`reads ${c.name} from "${c.key}"`, async () => {
      const { api, session } = client(() => json({ [c.key]: [{ marker: c.key }] }));
      const rows = await c.call(api);
      expect(rows).toEqual([{ marker: c.key }]);
      expect(session.calls[0].path).toBe(c.path);
    });

    it(`treats a missing ${c.name} list as empty, not as a crash`, async () => {
      // A degraded read can answer with the envelope and no rows.
      const { api } = client(() => json({}));
      await expect(c.call(api)).resolves.toEqual([]);
    });
  }
});

describe('single objects are bare — except kill', () => {
  it('kill returns the ban under "ban"', async () => {
    const ban = { id: 'b1', state: 'active' };
    const { api, session } = client(() => json({ ban }, 201));
    await expect(api.kill('ABC123', { reason: 'terms' })).resolves.toEqual({ ban });
    expect(session.calls[0].path).toBe('api/v1/broadcasts/ABC123/kill');
  });

  it('createBan returns the Ban itself', async () => {
    const ban = { id: 'b2', state: 'active', target: { type: 'ip', value: '203.0.113.7/32' } };
    const { api } = client(() => json(ban, 201));
    await expect(
      api.createBan({ target: { type: 'ip', value: 'publisher' }, expiresAt: null, reason: 'x' }),
    ).resolves.toEqual(ban);
  });

  it('unban is a 204 with no body to parse', async () => {
    const { api, session } = client(() => new Response(null, { status: 204 }));
    await expect(api.unban('b1')).resolves.toBeUndefined();
    expect(session.calls[0].init.method).toBe('DELETE');
  });
});

describe('the events cursor', () => {
  it('carries nextAfterId through, null included', async () => {
    const { api } = client(() => json({ events: [{ id: 9 }], nextAfterId: null }));
    await expect(api.events()).resolves.toEqual({ events: [{ id: 9 }], nextAfterId: null });
  });

  it('sends afterId only when paging', async () => {
    const { api, session } = client(() => json({ events: [], nextAfterId: null }));
    await api.events();
    await api.events(8);
    expect(session.calls[0].path).toBe('api/v1/events?limit=50');
    expect(session.calls[1].path).toBe('api/v1/events?limit=50&afterId=8');
  });
});

describe('the refusal envelope', () => {
  it('surfaces the API’s own code and message', async () => {
    const { api } = client(() =>
      json({ error: { code: 'source_immutable', message: 'defined in the chart values' } }, 409),
    );
    await expect(api.deleteWebhook('w1')).rejects.toMatchObject({
      status: 409,
      code: 'source_immutable',
      message: 'defined in the chart values',
    });
  });

  it('carries the ban on 409 duplicate_active', async () => {
    const ban = { id: 'existing', state: 'active' };
    const { api } = client(() =>
      json({ error: { code: 'duplicate_active', message: 'already banned' }, ban }, 409),
    );
    const err = await api
      .createBan({ target: { type: 'broadcastId', value: 'A' }, expiresAt: null, reason: 'x' })
      .catch((e: unknown) => e as ApiError);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe('duplicate_active');
    expect((err as ApiError).ban).toEqual(ban);
  });

  it('carries the ban on 502 projection_failed — the row IS committed', async () => {
    const ban = { id: 'committed', state: 'active' };
    const { api } = client(() =>
      json({ error: { code: 'projection_failed', message: 'could not write the CR' }, ban }, 502),
    );
    const err = await api
      .createBan({ target: { type: 'broadcastId', value: 'A' }, expiresAt: null, reason: 'x' })
      .catch((e: unknown) => e as ApiError);
    expect((err as ApiError).status).toBe(502);
    expect((err as ApiError).code).toBe('projection_failed');
    expect((err as ApiError).ban).toEqual(ban);
  });

  it('falls back to the status when there is no JSON body at all', async () => {
    const { api } = client(() => new Response('gateway timeout', { status: 504 }));
    await expect(api.relays()).rejects.toMatchObject({ status: 504, code: '', ban: null });
  });
});
