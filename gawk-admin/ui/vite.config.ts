import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The build output is embedded into the gawk-admin binary (Go `embed`) and
// served from its single listener alongside `/api/v1` (docs/42 §4.6). Three
// consequences shape this config:
//
//  * `base: './'` — relative asset URLs, so the page works wherever it is
//    mounted: `/`, a port-forward, or an Ingress sub-path. The OIDC redirect
//    URI is derived from `window.location` for the same reason.
//  * NOTHING may be fetched from another origin at runtime. The portal's CSP
//    is `default-src 'self'; connect-src 'self' <issuer origin>` (§4.8): the
//    IdP is the single sanctioned external origin, and it is sanctioned for
//    XHR only. A CDN script tag or a webfont would be blocked outright, so the
//    embedded-assets rule is a hard constraint here, not a preference — and a
//    Go test asserts it against the built output.
//  * Stable, unhashed filenames (see below).
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    // Straight into the Go package that embeds it. `go:embed` cannot reach a
    // parent directory, so the output has to live beside the embedding file
    // rather than under ui/.
    outDir: '../internal/portal/dist',
    // NOT emptied: the directory carries one committed file (README.md) that
    // keeps `//go:embed dist` compilable on a fresh clone, and wiping it would
    // make `go build ./...` fail until someone had run npm.
    emptyOutDir: false,
    rollupOptions: {
      output: {
        // STABLE names, no content hash. Hashing exists for far-future CDN
        // caching; these assets are embedded in the binary and served
        // `no-store` (a moderation console showing yesterday's bundle after a
        // redeploy is a console that lies about what it is acting on). Stable
        // names also mean a rebuild overwrites in place, so nothing goes stale
        // in a directory that is never emptied.
        entryFileNames: 'assets/app.js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/app.[ext]',
        manualChunks: undefined,
      },
    },
  },
  // `npm run dev` against a REAL backend. Unlike telemetry's dashboard there is
  // no credential to inject here: the portal authenticates with an OIDC bearer
  // token the SPA obtains itself, so the dev server only has to forward the
  // API and the unauthenticated `/auth/config` bootstrap.
  //
  //   kubectl -n production port-forward svc/gawk-admin 8090:8090
  //   npm run dev
  //
  // The IdP must list the dev origin as a valid redirect URI for the public
  // client, or the authorization request is refused before it reaches us.
  server: {
    proxy: Object.fromEntries(
      ['/api', '/auth', '/healthz', '/readyz'].map((path) => [
        path,
        {
          target: process.env.GAWK_ADMIN_TARGET ?? 'http://127.0.0.1:8090',
          changeOrigin: true,
        },
      ]),
    ),
  },
  // R41 (docs/43): what the coverage badge is measured over.
  //
  // `include` is spelled out rather than left to the provider. Vitest's v8
  // coverage otherwise reports only the files a test actually imported, so a
  // module with no test at all would be missing from the DENOMINATOR instead
  // of counted as uncovered — the one thing a coverage number must not do.
  //
  // main.tsx is the only exclusion beyond the tests themselves: it is the
  // `createRoot` bootstrap, which runs in a browser and asserts nothing.
  test: {
    coverage: {
      provider: 'v8',
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/**/*.test.{ts,tsx}', 'src/main.tsx'],
      reporter: ['text-summary', 'json-summary'],
      reportsDirectory: 'coverage',
    },
  },
});
