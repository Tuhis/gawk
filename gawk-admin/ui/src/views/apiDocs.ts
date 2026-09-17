// The Redoc configuration, separated from the view that mounts it (R48,
// docs/49 D5, revised 2026-09-17).
//
// It is its own module because the thing worth testing here — that the page
// reads this deployment's own document and nothing off-origin — is a property
// of this object, not of React. Asserting it against a 1.1 MB third-party
// bundle rendered in jsdom would test the bundle.
//
// **There is no token here, and that is the point of the change.** Swagger UI
// executed requests, so it had to be handed the operator's access token
// through a `requestInterceptor`, with a same-origin guard so a misconfigured
// `-external-url` could not leak it. Redoc renders and does not execute, so
// the token never reaches the third-party bundle at all. The page lost "Try it
// out"; it also lost the only reason a third-party script in the
// security-critical SPA ever saw a credential.

/**
 * Where the contract is served.
 *
 * RELATIVE — no leading slash — exactly like `client.ts`'s `BASE` and for the
 * same reason: `vite.config.ts` sets `base: './'`, so the whole SPA works
 * wherever it is mounted, and a root-absolute path here would make this one
 * page the only thing that breaks under an Ingress sub-path. Both the document
 * fetch and the link on the page would go to the origin root and 404.
 */
export const OPENAPI_URL = 'api/v1/openapi.json';

/**
 * The options handed to `Redoc.init`.
 *
 * Each of these is either a default worth pinning or a deliberate narrowing:
 *
 *   - `hideDownloadButton` is FALSE. Fetching the raw document is exactly what
 *     a bot author came to do, and the served copy is the one carrying this
 *     deployment's base URL and role names.
 *   - `expandResponses: '200,201'` puts the success shape on screen without a
 *     click, which is the shape somebody is reading for.
 *   - `nativeScrollbars` avoids Redoc's custom scrollbar, which fights the
 *     portal's own scrolling on a phone — and the portal is read from one
 *     (docs/42 §10).
 */
export function redocOptions(): Record<string, unknown> {
  return {
    // OFF, and not by preference: Redoc builds its search index in a worker
    // created from a `blob:` URL, and the portal's CSP is `default-src
    // 'self'` with no `worker-src`, so the browser refuses it and logs a
    // violation on every visit. The alternative is widening the CSP of the
    // page that holds an operator's access token to get search on a
    // documentation page (docs/42 §4.8 treats that CSP as load-bearing), and
    // that is the wrong way round. The sidebar lists every operation, and
    // the browser's own find-in-page works on the rendered document.
    disableSearch: true,
    hideDownloadButton: false,
    expandResponses: '200,201',
    nativeScrollbars: true,
    // Redoc renders `x-gawk-sensitive`, `x-gawk-roles` and `x-gawk-requires`
    // as extension rows rather than hiding them, which is what we want: they
    // are the parts of the document a consumer most needs to see.
    showExtensions: true,
    // The portal's own type scale, so the page does not read as a foreign
    // document embedded in it.
    theme: {
      typography: {
        fontSize: '15px',
        fontFamily: 'inherit',
        headings: { fontFamily: 'inherit' },
        code: { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace' },
      },
      sidebar: { width: '14rem' },
    },
  };
}
