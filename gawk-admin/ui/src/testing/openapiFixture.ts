// A slice of the real contract, in the shapes it actually uses, for the
// console's tests. Test-only, like harness.tsx: nothing in app code imports it.

import type { Document } from '../views/apiConsole.ts';

/** A slice of the real contract, in the shapes it actually uses. */
export const DOC: Document = {
  servers: [{ url: 'https://admin.gawk.example/' }],
  // The tag order is the sidebar's. `paths` below is deliberately NOT in this
  // order (nor alphabetical): the served copy is sorted by Go, and the
  // console must not depend on which order it gets.
  tags: [{ name: 'identity' }, { name: 'bans' }, { name: 'webhooks' }],
  paths: {
    '/api/v1/openapi.json': {
      get: { operationId: 'getOpenAPIDocument', tags: ['identity'], summary: 'Fetch', 'x-gawk-roles': [], responses: { '200': {} } },
    },
    '/api/v1/webhooks/{name}/test': {
      post: {
        operationId: 'testWebhook',
        tags: ['webhooks'],
        summary: 'Test-send',
        'x-gawk-roles': ['operator'],
        parameters: [{ name: 'name', in: 'path', required: true, schema: { type: 'string' } }],
        requestBody: { content: { 'application/json': { schema: { type: 'object' } } } },
        responses: { '200': {} },
      },
    },
    '/api/v1/bans': {
      get: {
        operationId: 'listBans',
        tags: ['bans'],
        summary: 'List bans',
        'x-gawk-roles': ['operator'],
        parameters: [
          // A REQUIRED query parameter. The real contract has none today; the
          // fixture carries one so the console's handling of it is pinned
          // before the first one lands.
          { name: 'scope', in: 'query', required: true, schema: { type: 'string' } },
          { name: 'state', in: 'query', schema: { type: 'string', enum: ['active', 'all'], default: 'active' } },
          { name: 'limit', in: 'query', schema: { type: 'integer', default: 50, maximum: 500 } },
          { name: 'afterId', in: 'query', schema: { type: 'string', format: 'uuid' } },
        ],
        responses: {
          '200': { 'x-gawk-sensitive': true, 'x-gawk-sensitive-reason': 'A ban target may be a raw broadcast ID.' },
          '400': {},
        },
      },
      post: {
        operationId: 'createBan',
        tags: ['bans'],
        summary: 'Create a ban',
        'x-gawk-roles': ['operator'],
        requestBody: {
          required: true,
          content: { 'application/json': { schema: { $ref: '#/components/schemas/CreateBanRequest' }, example: { reason: 'x', target: { type: 'id', value: 'ABC234' } } } },
        },
        responses: { '201': {} },
      },
    },
    '/api/v1/bans/{id}': {
      delete: {
        operationId: 'removeBan',
        tags: ['bans'],
        summary: 'Remove a ban',
        'x-gawk-roles': ['operator'],
        parameters: [{ name: 'id', in: 'path', required: true, description: 'The ban.', schema: { $ref: '#/components/schemas/BanId' } }],
        responses: { '204': {} },
      },
    },
    '/api/v1/me': {
      get: { operationId: 'getMe', tags: ['identity'], summary: 'Who am I', 'x-gawk-roles': ['operator'], responses: { '200': {} } },
    },
  },
  components: {
    schemas: {
      BanId: { type: 'string', format: 'uuid' },
      CreateBanRequest: { type: 'object' },
    },
  },
};

