import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The build output is embedded into the gawk-telemetry binary (Go `embed`) and
// served from its read listener. Two consequences shape this config:
//
//  * `base: './'` — relative asset URLs, so the page works wherever it is
//    mounted: `/`, a port-forward, or an Ingress sub-path. An absolute base
//    would break the port-forward workflow the dashboard exists to support.
//  * NOTHING may be fetched from another origin at runtime (docs/33 §4.8.4).
//    Vite emits only local files, so this holds by construction — but it is a
//    hard constraint, not a preference, and a Go test asserts it against the
//    built output. The one exception is OIDC mode's identity provider, which
//    the SPA talks to directly and which the read listener's CSP sanctions in
//    `connect-src` only (docs/55 D5) — no script, style or font from it; a
//    second Go test checks the bundle against that CSP.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    // Straight into the Go package that embeds it. `go:embed` cannot reach a
    // parent directory, so the output has to live beside the embedding file
    // rather than under ui/.
    outDir: '../internal/dashboard/dist',
    // NOT emptied: the directory carries one committed file (README.md) that
    // keeps `//go:embed dist` compilable on a fresh clone, and wiping it would
    // make `go build ./...` fail until someone had run npm.
    emptyOutDir: false,
    rollupOptions: {
      output: {
        // STABLE names, no content hash. Hashing exists for far-future CDN
        // caching; these assets are embedded in the binary and served
        // `no-store` (an ops page showing yesterday's bundle after a redeploy
        // is a page that lies about what it is measuring). Stable names also
        // mean a rebuild overwrites in place, so nothing goes stale in a
        // directory that is never emptied.
        entryFileNames: 'assets/app.js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/app.[ext]',
        manualChunks: undefined,
      },
    },
  },
  // `npm run dev` against a REAL backend — the developer loop this whole change
  // is meant to keep fast. Point it at a port-forwarded read listener:
  //
  //   kubectl -n production port-forward svc/gawk-telemetry-read 8081:8081
  //   GAWK_TM_AUTH='admin:secret' npm run dev
  //
  // The proxy injects the basic-auth header so the browser never prompts, and
  // so no credential is typed into a page that is being hot-reloaded.
  //
  // In OIDC mode (docs/55 D1) there is no credential to inject: the SPA runs
  // the code+PKCE flow itself, so GAWK_TM_AUTH is left unset and the proxy
  // only forwards — `/auth/config` included, which is how the page learns the
  // mode. The IdP must list the dev origin as a redirect URI for the client.
  server: {
    // `@gawk/oidc-session` is a `file:` link to ../../common-ts (docs/55 D3),
    // and Vite resolves it to that real path — outside this project, where the
    // dev server (and vitest, which runs the package's own tests from here)
    // refuses to read by default.
    fs: { allow: ['.', '../../common-ts'] },
    proxy: Object.fromEntries(
      ['/live', '/v1', '/mcp', '/auth'].map((path) => [
        path,
        {
          target: process.env.GAWK_TM_TARGET ?? 'http://127.0.0.1:8081',
          changeOrigin: true,
          configure: (proxy: { on: (e: string, cb: (p: unknown, r: unknown) => void) => void }) => {
            const auth = process.env.GAWK_TM_AUTH;
            if (!auth) return;
            proxy.on('proxyReq', (proxyReq) => {
              (proxyReq as { setHeader: (k: string, v: string) => void }).setHeader(
                'Authorization',
                `Basic ${Buffer.from(auth).toString('base64')}`,
              );
            });
          },
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
  //
  // The shared session package (docs/55 D3) is counted too, exactly as
  // gawk-admin counts it: it is bundled into this SPA, so it is part of what
  // the badge measures. `allowExternal` is what lets v8 count a file outside
  // this project; its harness (testing.ts) is test code and excluded.
  test: {
    // The shared package is tested where it lives, by every consumer's run
    // (docs/55 D3): it has no toolchain of its own, so a change to it is
    // exercised against each SPA that bundles it.
    include: ['src/**/*.test.{ts,tsx}', '../../common-ts/oidc-session/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/**/*.{ts,tsx}', '**/common-ts/oidc-session/*.ts'],
      exclude: [
        'src/**/*.test.{ts,tsx}',
        'src/main.tsx',
        '**/common-ts/oidc-session/*.test.ts',
        '**/common-ts/oidc-session/testing.ts',
      ],
      allowExternal: true,
      reporter: ['text-summary', 'json-summary'],
      reportsDirectory: 'coverage',
    },
  },
});
