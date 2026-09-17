// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { OPENAPI_URL, redocOptions } from './apiDocs.ts';

describe('the API page’s Redoc configuration (R48, docs/49 D5)', () => {
  it('reads the document from this deployment, by a relative path', () => {
    // RELATIVE, like every path in client.ts. This is the property that makes
    // the page work under an Ingress sub-path.
    expect(OPENAPI_URL).toBe('api/v1/openapi.json');
    expect(OPENAPI_URL.startsWith('/')).toBe(false);

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
  });

  it('leaves the download button on', () => {
    // Fetching the raw document is exactly what a bot author came to do, and
    // the served copy is the one carrying this deployment's base URL and role
    // names rather than the repository's symbolic ones.
    expect(redocOptions().hideDownloadButton).toBe(false);
  });

  it('keeps the gawk extensions visible, and search off', () => {
    const opts = redocOptions();
    // Search builds its index in a worker created from a `blob:` URL, which
    // the portal's CSP refuses. Turning it back on means widening the CSP of
    // the page that holds an operator's token — see apiDocs.ts.
    expect(opts.disableSearch).toBe(true);
    // x-gawk-roles, x-gawk-requires and x-gawk-sensitive are the parts of the
    // document a consumer most needs to see; hiding them would leave the page
    // less informative than curl.
    expect(opts.showExtensions).toBe(true);
  });

  // The security property that came free with dropping Swagger UI: Redoc does
  // not execute requests, so the third-party bundle is never handed the
  // operator's access token. Nothing here may reintroduce one.
  it('hands the renderer no credential of any kind', () => {
    const serialized = JSON.stringify(redocOptions());
    for (const forbidden of ['token', 'authorization', 'bearer', 'requestinterceptor']) {
      expect(serialized.toLowerCase()).not.toContain(forbidden);
    }
    // And the options are plain values — no function that could close over a
    // session and smuggle one in later.
    for (const value of Object.values(redocOptions())) {
      expect(typeof value).not.toBe('function');
    }
  });
});
