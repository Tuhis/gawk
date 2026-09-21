import { describe, expect, it } from 'vitest';

import {
  buildRequestPath,
  curlFor,
  hashForOperation,
  listOperations,
  operationFromHash,
  resolveRef,
} from './apiConsole.ts';
import { DOC } from '../testing/openapiFixture.ts';

const ops = listOperations(DOC);
const byId = (id: string) => ops.find((o) => o.id === id)!;

describe('listOperations', () => {
  // Grouped by the document's tag order, not by the order `paths` happens to
  // arrive in: the served JSON is sorted by Go, and the fixture is shuffled.
  it('lists every operation grouped in tag order, with method, tag and role', () => {
    expect(ops.map((o) => `${o.method} ${o.path}`)).toEqual([
      'GET /api/v1/openapi.json',
      'GET /api/v1/me',
      'GET /api/v1/bans',
      'POST /api/v1/bans',
      'DELETE /api/v1/bans/{id}',
      'POST /api/v1/webhooks/{name}/test',
    ]);
    expect(byId('getMe').roles).toEqual(['operator']);
    expect(byId('getOpenAPIDocument').roles).toEqual([]);
    expect(byId('listBans').tag).toBe('bans');
  });

  it('reads parameters, following $ref into components, and marks path ones required', () => {
    const remove = byId('removeBan');
    expect(remove.params).toEqual([
      { name: 'id', in: 'path', required: true, description: 'The ban.', enum: null, hint: 'uuid' },
    ]);
    const list = byId('listBans');
    expect(list.params.map((p) => p.name)).toEqual(['scope', 'state', 'limit', 'afterId']);
    expect(list.params[0].required).toBe(true);
    expect(list.params[1].enum).toEqual(['active', 'all']);
    expect(list.params[1].hint).toBe('default active');
    expect(list.params[2].hint).toBe('default 50, ≤ 500');
    expect(list.params[3].required).toBe(false);
  });

  it('pre-fills the body from the documented example, and with {} when there is none', () => {
    expect(JSON.parse(byId('createBan').bodyExample!)).toEqual({ reason: 'x', target: { type: 'id', value: 'ABC234' } });
    expect(byId('testWebhook').bodyExample).toBe('{}');
    expect(byId('getMe').bodyExample).toBeNull();
  });

  it('carries the x-gawk-sensitive reason of a response', () => {
    expect(byId('listBans').sensitive).toBe('A ban target may be a raw broadcast ID.');
    expect(byId('getMe').sensitive).toBeNull();
  });
});

describe('buildRequestPath', () => {
  // The whole security argument of the console rests on this: the path is
  // relative, so the token attached by authorizedFetch cannot leave the origin.
  it('is relative — api/v1/…, no leading slash', () => {
    expect(buildRequestPath(byId('getMe'), {})).toBe('api/v1/me');
    expect(buildRequestPath(byId('getMe'), {}).startsWith('/')).toBe(false);
  });

  it('encodes path parameters per segment', () => {
    expect(buildRequestPath(byId('removeBan'), { id: 'a/b c' })).toBe('api/v1/bans/a%2Fb%20c');
  });

  it('refuses an empty path parameter rather than sending bans/', () => {
    expect(() => buildRequestPath(byId('removeBan'), {})).toThrow(/id is required/);
    expect(() => buildRequestPath(byId('removeBan'), { id: '' })).toThrow(/id is required/);
  });

  it('sends only the optional query parameters that hold something', () => {
    expect(buildRequestPath(byId('listBans'), { scope: 'x', state: 'all', limit: '', afterId: '' })).toBe(
      'api/v1/bans?scope=x&state=all',
    );
    expect(buildRequestPath(byId('listBans'), { scope: 'x' })).toBe('api/v1/bans?scope=x');
    expect(buildRequestPath(byId('listBans'), { limit: '5', state: 'all', scope: 'x' })).toBe(
      'api/v1/bans?scope=x&state=all&limit=5',
    );
  });

  // The field says "required"; the request must not quietly leave without
  // it. Same rule as a path parameter — the server's 400 is not the place
  // to learn a field was mandatory. (Review of PR #337.)
  it('refuses an empty REQUIRED query parameter like an empty path one', () => {
    expect(() => buildRequestPath(byId('listBans'), {})).toThrow(/scope is required/);
    expect(() => buildRequestPath(byId('listBans'), { scope: '', state: 'all' })).toThrow(/scope is required/);
  });
});

describe('curlFor', () => {
  it('targets the served base URL with a $TOKEN placeholder, never a real token', () => {
    const line = curlFor(DOC, byId('getMe'), 'api/v1/me', null);
    expect(line).toBe(`curl -H 'Authorization: Bearer $TOKEN' 'https://admin.gawk.example/api/v1/me'`);
  });

  it('spells the method, the content type and the body for a mutation', () => {
    const line = curlFor(DOC, byId('createBan'), 'api/v1/bans', '{"reason":"it\'s"}');
    expect(line).toBe(
      `curl -X POST -H 'Authorization: Bearer $TOKEN' -H 'Content-Type: application/json' -d '{"reason":"it'\\''s"}' 'https://admin.gawk.example/api/v1/bans'`,
    );
  });

  it('falls back to a root-relative URL when the document names no server', () => {
    expect(curlFor({}, byId('getMe'), 'api/v1/me', null)).toContain(`'/api/v1/me'`);
  });
});

describe('the Redoc hash', () => {
  it('names an operation in the shapes Redoc writes', () => {
    expect(operationFromHash('#tag/bans/operation/createBan')).toBe('createBan');
    expect(operationFromHash('#operation/getMe')).toBe('getMe');
    expect(operationFromHash('#tag/bans')).toBeNull();
    expect(operationFromHash('#/api')).toBeNull();
    expect(operationFromHash('')).toBeNull();
  });

  it('round-trips through hashForOperation', () => {
    const hash = hashForOperation(byId('createBan'));
    expect(hash).toBe('#tag/bans/operation/createBan');
    expect(operationFromHash(hash)).toBe('createBan');
  });
});

describe('resolveRef', () => {
  it('follows local pointers and returns {} for anything else', () => {
    expect(resolveRef(DOC, { $ref: '#/components/schemas/BanId' })).toEqual({ type: 'string', format: 'uuid' });
    expect(resolveRef(DOC, { $ref: '#/components/schemas/Missing' })).toEqual({});
    expect(resolveRef(DOC, { $ref: 'https://elsewhere/x.json' })).toEqual({});
    expect(resolveRef(DOC, { type: 'integer' })).toEqual({ type: 'integer' });
    expect(resolveRef(DOC, null)).toEqual({});
  });
});
