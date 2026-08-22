// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { ApiClient, ApiError, enforcementNotice } from './client.ts';
import type { Ban } from './types.ts';
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

  // Broadcasts keeps its whole envelope: the coverage counters are what let
  // the view distinguish a quiet fleet from an unreachable one.
  it('reads broadcasts WITH the scan-coverage counters', async () => {
    const { api, session } = client(() =>
      json({ broadcasts: [{ marker: 'broadcasts' }], podsResolved: 3, podsAnswered: 2 }),
    );
    await expect(api.broadcasts()).resolves.toEqual({
      broadcasts: [{ marker: 'broadcasts' }],
      podsResolved: 3,
      podsAnswered: 2,
    });
    expect(session.calls[0].path).toBe('api/v1/broadcasts');
  });

  it('treats a missing broadcasts list and counters as empty full coverage, not a crash', async () => {
    // A degraded read — or an older binary that predates the counters — can
    // answer with no rows and no coverage; 0/0 flags nothing.
    const { api } = client(() => json({}));
    await expect(api.broadcasts()).resolves.toEqual({
      broadcasts: [],
      podsResolved: 0,
      podsAnswered: 0,
    });
  });
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
    // `res.json()` on an empty body throws, so the 204 must not be parsed at
    // all — null, not undefined-from-a-swallowed-error.
    const { api, session } = client(() => new Response(null, { status: 204 }));
    await expect(api.unban('b1')).resolves.toBeNull();
    expect(session.calls[0].init.method).toBe('DELETE');
  });
});

// The middle outcome: the `bans` row is committed, its Ban CR is not. That is
// a SUCCESS — the record is durable and the reconciler finishes within a
// minute — so the client must resolve, not reject, and must hand the caller
// the ban so it has the id it must NOT re-submit.
describe('202 Accepted: recorded, not yet in sync', () => {
  const pending = {
    id: 'committed',
    state: 'active',
    enforcement: { inSync: false, detail: 'recorded but NOT enforced yet' },
  };

  it('kill resolves with the ban under "ban"', async () => {
    const { api } = client(() => json({ ban: pending }, 202));
    await expect(api.kill('ABC123', { reason: 'terms' })).resolves.toEqual({ ban: pending });
  });

  it('createBan resolves with the bare ban', async () => {
    const { api } = client(() => json(pending, 202));
    await expect(
      api.createBan({ target: { type: 'broadcastId', value: 'A' }, expiresAt: null, reason: 'x' }),
    ).resolves.toEqual(pending);
  });

  it('unban resolves with the removed ban, unlike its 204', async () => {
    const removed = {
      ...pending,
      state: 'removed',
      enforcement: { inSync: false, detail: 'lifted in the record, STILL banned' },
    };
    const { api } = client(() => json(removed, 202));
    await expect(api.unban('b1')).resolves.toEqual(removed);
  });

  it('enforcementNotice reads only an out-of-sync ban', async () => {
    expect(enforcementNotice(pending as Ban)).toBe('recorded but NOT enforced yet');
    // A 201/204 ban and every list row carry no `enforcement` at all.
    expect(enforcementNotice({ id: 'x' } as Ban)).toBeNull();
    expect(enforcementNotice({ id: 'x', enforcement: { inSync: true } } as Ban)).toBeNull();
    expect(enforcementNotice(null)).toBeNull();
    // A detail-less 202 still has to say something.
    const bare = { id: 'x', enforcement: { inSync: false } } as Ban;
    expect(enforcementNotice(bare)).toMatch(/do not re-submit/i);
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

  it('falls back to the status when there is no JSON body at all', async () => {
    const { api } = client(() => new Response('gateway timeout', { status: 504 }));
    await expect(api.relays()).rejects.toMatchObject({ status: 504, code: '', ban: null });
  });
});
