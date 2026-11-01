// The Redoc configuration, separated from the view that mounts it (R48,
// docs/49 D5, revised 2026-09-17 and, for Redoc 3, 2026-11-01).
//
// It is its own module because the things worth testing here — that the page
// reads this deployment's own document, that the renderer is handed no
// credential, and that it phones nobody — are properties of plain values, not
// of React. Asserting them against a 2 MB third-party bundle rendered in jsdom
// would test the bundle.
//
// **There is no token here, and that is the point.** Swagger UI executed
// requests, so it had to be handed the operator's access token through a
// `requestInterceptor`, with a same-origin guard so a misconfigured
// `-external-url` could not leak it. Redoc renders and does not execute, so
// the token never reaches the third-party bundle at all.
//
// **There is no theme here either, since Redoc 3.** Redoc 2 took a theme
// object, which this module built from the portal's live CSS variables. Redoc
// 3 dropped that API and themes through CSS custom properties instead, so the
// portal's palette now reaches it the way it reaches every other view: in CSS
// (`ApiView.css`).

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
 * Where the event contract is served (R51, docs/52 D3): the AsyncAPI
 * catalogue of every event this deployment emits. Relative for the same
 * reason as OPENAPI_URL. The page only links to it — rendering it too would
 * put a second large renderer on a page that already embeds Redoc, and the
 * document reads fine raw (docs/52 §2, Rejected).
 */
export const ASYNCAPI_URL = 'api/v1/asyncapi.json';

/**
 * Redoc's routing base on this page, inside the portal's hash.
 *
 * Redoc 3 is a react-router application: every page of the reference — an
 * operation, a tag, a section — is a ROUTE, `<base>/<tag>/<operationid>`.
 * Left to itself it mounts a `BrowserRouter` and writes those routes into
 * `location.pathname`, which the portal cannot have: the portal is hash-routed
 * (router.ts says why), and a pathname rewrite would break a reload under an
 * Ingress sub-path and the OIDC redirect URI with it. So `ApiView` hands Redoc
 * a `HashRouter`, and with this base every docs URL is `#/api/...` — a route
 * `parseHash` already resolves to this page, so reloads, bookmarks, the Back
 * button and a shared link all land on the operation they name.
 */
export const DOCS_BASE_PATH = '/api';

/**
 * Redoc's slug for a name in a docs route: lower case, whitespace to `-`.
 *
 * Restated rather than imported — Redoc exports no slug helper — and narrow
 * on purpose: it is only ever applied to this document's tags (single lower
 * case words) and operation IDs (camelCase identifiers), which is the whole
 * input it has to agree with Redoc on. A browser pass checked every
 * operation's route against the links Redoc renders.
 */
export function docsSlug(name: string): string {
  return name.toLowerCase().trim().replace(/\s+/g, '-');
}

/**
 * The Redoc 3 route for a Redoc 2 fragment, or null if `hash` is not one.
 *
 * Redoc 2 wrote `#tag/<tag>/operation/<id>`, `#operation/<id>` and
 * `#section/<Name>` into the address bar, and operators bookmarked and shared
 * them (`router.ts` still resolves those roots to this page for that reason).
 * Redoc 3 does not understand them, so `ApiView` rewrites one to the route it
 * means before Redoc reads the location. `#operation/<id>` names no tag, so
 * it lands on the top of the reference rather than guessing one.
 */
export function legacyDocsRoute(hash: string): string | null {
  const raw = hash.replace(/^#/, '');
  if (raw.startsWith('/')) return null;
  const segments = raw.split('?')[0].split('/').filter(Boolean).map(decodeSegment);
  switch (segments[0]) {
    case 'tag':
      if (!segments[1]) return DOCS_BASE_PATH;
      if (segments[2] === 'operation' && segments[3]) {
        return `${DOCS_BASE_PATH}/${docsSlug(segments[1])}/${docsSlug(segments[3])}`;
      }
      return `${DOCS_BASE_PATH}/${docsSlug(segments[1])}`;
    case 'section':
      return segments[1]
        ? `${DOCS_BASE_PATH}/section/${docsSlug(segments[1].replace(/-/g, ' '))}`
        : DOCS_BASE_PATH;
    case 'operation':
      return DOCS_BASE_PATH;
    default:
      return null;
  }
}

function decodeSegment(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

/**
 * The options handed to Redoc.
 *
 *   - `disableTelemetry` is TRUE. Redoc 3 reports usage to Redocly's
 *     collector by default. The portal's CSP would refuse the request — its
 *     `connect-src` is this origin and the IdP — but a moderation console
 *     does not attempt to phone a vendor and rely on being stopped.
 *     `vite.config.ts` also strips the collector URL from the bundle, so the
 *     option is not the only thing standing between the page and the vendor.
 *   - `hideDownloadButtons` is FALSE. Fetching the raw document is exactly
 *     what a bot author came to do, and the served copy is the one carrying
 *     this deployment's base URL and role names.
 *   - `showExtensions` keeps `x-gawk-roles`, `x-gawk-requires` and
 *     `x-gawk-sensitive` on the page: they are what a consumer most needs.
 *
 * Every value is plain data: no callback that could close over a session.
 */
export function redocOptions(): Record<string, unknown> {
  return {
    disableTelemetry: true,
    hideDownloadButtons: false,
    showExtensions: true,
  };
}
