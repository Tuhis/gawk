// `swagger-ui-dist` ships no types: it is a prebuilt bundle, not a library.
//
// This declares only what ApiView actually calls, deliberately — a fuller
// hand-written declaration would be a second, drifting copy of somebody else's
// API, and `apiDocs.ts` already owns the shape of the options we pass.
declare module 'swagger-ui-dist/swagger-ui-es-bundle.js' {
  const SwaggerUIBundle: (options: Record<string, unknown>) => unknown;
  export default SwaggerUIBundle;
}
