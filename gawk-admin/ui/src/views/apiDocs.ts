// The Swagger UI configuration, separated from the view that mounts it (R48,
// docs/49 D5).
//
// It is its own module because the two things worth testing here — that the
// caller's token is attached to a "Try it out" request, and that nothing
// off-origin is ever fetched — are properties of this object, not of React.
// Asserting them against a 1.6 MB third-party bundle rendered in jsdom would
// test the bundle.

/**
 * Where the contract is served. RELATIVE, like every path in `client.ts`: the
 * page is served by the binary that answers it, so this works identically on
 * `/`, on a port-forward and under an Ingress sub-path.
 */
export const OPENAPI_URL = '/api/v1/openapi.json';

/** The shape of a request Swagger UI hands to `requestInterceptor`. */
export interface SwaggerRequest {
  url: string;
  headers?: Record<string, string>;
  [key: string]: unknown;
}

/**
 * Attaches the caller's bearer token to a same-origin request.
 *
 * Two rules, and the second is the one worth writing down: the token goes on
 * requests to THIS origin and nowhere else. Swagger UI will happily execute a
 * request against whatever `servers[0].url` says, and a deployment whose
 * `-external-url` has been pointed somewhere else must not become a way to
 * hand an operator's access token to that somewhere else.
 */
export function authorizeRequest(req: SwaggerRequest, token: string | null): SwaggerRequest {
  if (!token || !isSameOrigin(req.url)) return req;
  return { ...req, headers: { ...(req.headers ?? {}), Authorization: `Bearer ${token}` } };
}

function isSameOrigin(url: string): boolean {
  if (typeof window === 'undefined') return false;
  try {
    return new URL(url, window.location.href).origin === window.location.origin;
  } catch {
    // An unparseable URL is not this origin.
    return false;
  }
}

/**
 * The options handed to `SwaggerUIBundle`.
 *
 * `validatorUrl: null` is not cosmetic. Swagger UI's default is to POST the
 * document to an online validator for a badge — an off-origin request from the
 * security-critical portal, which the CSP (`default-src 'self'`) would block
 * anyway, leaving a broken badge where the explanation should be. The document
 * is linted in CI; the page does not need a second opinion from the internet.
 */
export function swaggerOptions(
  domNode: HTMLElement,
  token: () => string | null,
): Record<string, unknown> {
  return {
    url: OPENAPI_URL,
    domNode,
    validatorUrl: null,
    // The operator navigated here from the portal's own nav; a second
    // top bar with a spec-URL box would only invite loading someone
    // else's document into this page.
    supportedSubmitMethods: ['get', 'post', 'put', 'delete'],
    docExpansion: 'list',
    defaultModelsExpandDepth: 0,
    persistAuthorization: false,
    tryItOutEnabled: true,
    requestInterceptor: (req: SwaggerRequest) => authorizeRequest(req, token()),
  };
}
