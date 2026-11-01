// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import {
  DOCS_BASE_PATH,
  OPENAPI_URL,
  docsSlug,
  legacyDocsRoute,
  redocOptions,
} from './apiDocs.ts';
import { parseHash } from '../router/router.ts';

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
    expect(redocOptions().hideDownloadButtons).toBe(false);
  });

  it('keeps the gawk extensions visible', () => {
    // x-gawk-roles, x-gawk-requires and x-gawk-sensitive are the parts of the
    // document a consumer most needs to see; hiding them would leave the page
    // less informative than curl.
    expect(redocOptions().showExtensions).toBe(true);
  });

  // Redoc 3 reports usage to Redocly's collector unless told not to. The CSP
  // would refuse the request, but a moderation console does not attempt to
  // phone a vendor and rely on being stopped. vite.config.ts also strips the
  // collector URL, and internal/portal's asset test asserts that half.
  it('turns Redoc’s telemetry off', () => {
    expect(redocOptions().disableTelemetry).toBe(true);
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

describe('the docs routes (docs/49 D5, revised 2026-11-01)', () => {
  // Redoc 3 is routed by the API view's HashRouter under this base, so every
  // docs URL is `#/api/...`. The portal's own router has to agree that such a
  // URL is the API page, or a reload of any operation lands on not-found.
  it('lives under a base the portal router resolves to the API page', () => {
    expect(DOCS_BASE_PATH).toBe('/api');
    expect(parseHash(`#${DOCS_BASE_PATH}`).view).toBe('api');
    expect(parseHash(`#${DOCS_BASE_PATH}/bans/createban`).view).toBe('api');
  });

  // Checked against a real render: every operation route this produces for
  // the served document matched a link Redoc drew.
  it('slugs a name the way Redoc does for this document', () => {
    expect(docsSlug('createBan')).toBe('createban');
    expect(docsSlug('bans')).toBe('bans');
    expect(docsSlug('Ban mutations')).toBe('ban-mutations');
  });

  // Redoc 2 wrote these into the address bar, so operators have them
  // bookmarked; Redoc 3 does not understand them.
  it('maps a Redoc 2 fragment to the Redoc 3 route it means', () => {
    expect(legacyDocsRoute('#tag/bans/operation/createBan')).toBe('/api/bans/createban');
    expect(legacyDocsRoute('#tag/rooms')).toBe('/api/rooms');
    expect(legacyDocsRoute('#section/Ban-mutations')).toBe('/api/section/ban-mutations');
    expect(legacyDocsRoute('#section/Authentication')).toBe('/api/section/authentication');
    // No tag to route by: the top of the reference, rather than a guess.
    expect(legacyDocsRoute('#operation/killBroadcast')).toBe('/api');
    expect(legacyDocsRoute('#tag/')).toBe('/api');
  });

  it('leaves every other hash alone', () => {
    for (const hash of ['', '#', '#/api', '#/api/bans/createban', '#/bans', '#tagged/x']) {
      expect(legacyDocsRoute(hash), hash).toBeNull();
    }
  });
});
