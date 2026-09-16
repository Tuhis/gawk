// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { OPENAPI_URL, authorizeRequest, swaggerOptions } from './apiDocs.ts';
import type { SwaggerRequest } from './apiDocs.ts';

describe('the API page’s Swagger UI configuration (R48, docs/49 D5)', () => {
  it('attaches the caller’s token to a same-origin request', () => {
    const req: SwaggerRequest = { url: '/api/v1/me', headers: { Accept: 'application/json' } };
    const out = authorizeRequest(req, 'token-abc');
    expect(out.headers?.Authorization).toBe('Bearer token-abc');
    // And it does not lose the headers Swagger UI already set.
    expect(out.headers?.Accept).toBe('application/json');
    // The original is untouched: an interceptor that mutated its argument
    // would leak the token into whatever else holds a reference to it.
    expect(req.headers?.Authorization).toBeUndefined();
  });

  it('attaches nothing when there is no token', () => {
    const out = authorizeRequest({ url: '/api/v1/me' }, null);
    expect(out.headers?.Authorization).toBeUndefined();
  });

  // The rule worth a test of its own: Swagger UI executes against whatever
  // `servers[0].url` says, and that value comes from the deployment's
  // -external-url. A misconfigured or hostile one must not become a way to
  // hand an operator's access token to another origin.
  it('never sends the token off-origin', () => {
    for (const url of [
      'https://elsewhere.example/api/v1/me',
      'http://127.0.0.1:9999/collect',
      '//evil.example/api/v1/me',
    ]) {
      const out = authorizeRequest({ url }, 'token-abc');
      expect(out.headers?.Authorization, `${url} received the token`).toBeUndefined();
    }
  });

  it('reads the document from this deployment and asks no validator about it', () => {
    const node = document.createElement('div');
    const opts = swaggerOptions(node, () => 'token-abc');
    expect(opts.url).toBe(OPENAPI_URL);
    // RELATIVE, like every path in client.ts: a leading slash would make this
    // the one page that breaks under an Ingress sub-path.
    expect(OPENAPI_URL.startsWith('/')).toBe(false);
    expect(OPENAPI_URL).toBe('api/v1/openapi.json');
    expect(opts.domNode).toBe(node);
    // The sub-path claim, as arithmetic rather than assertion: under an
    // Ingress that mounts the portal at /admin/, the relative URL resolves
    // inside the deployment. A leading slash would resolve to the origin root
    // and 404 — and only on this page, since the rest of the SPA is relative.
    expect(new URL(OPENAPI_URL, 'https://host.example/admin/#/api').pathname).toBe(
      '/admin/api/v1/openapi.json',
    );
    expect(new URL(OPENAPI_URL, 'https://host.example/#/api').pathname).toBe(
      '/api/v1/openapi.json',
    );
    // Swagger UI's default POSTs the document to an online validator for a
    // badge. The CSP would block it; this is what makes it not happen at all.
    expect(opts.validatorUrl).toBeNull();
    // The token is never persisted — the in-memory holder stays the only copy.
    expect(opts.persistAuthorization).toBe(false);
  });

  it('reads the token at request time, not at mount time', () => {
    let token: string | null = null;
    const opts = swaggerOptions(document.createElement('div'), () => token);
    const intercept = opts.requestInterceptor as (r: SwaggerRequest) => SwaggerRequest;

    expect(intercept({ url: '/api/v1/me' }).headers?.Authorization).toBeUndefined();
    // A silent refresh happens between page load and the operator clicking
    // Execute; the request must carry the token they have NOW.
    token = 'refreshed';
    expect(intercept({ url: '/api/v1/me' }).headers?.Authorization).toBe('Bearer refreshed');
  });
});
