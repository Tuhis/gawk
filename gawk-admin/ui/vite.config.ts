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
/**
 * What Redoc's bundle would make the portal's CSP refuse, removed at build
 * time (R48, docs/49 D5; Redoc 3 revision 2026-11-01).
 *
 * The CSP (`default-src 'self'`, gawk-server/oidcauth/headers.go) blocks all
 * of these, so none of them could leak or run. But each one blocked is a
 * console error and a CSP violation on every visit to `#/api`, and the
 * off-origin URLs are references the repository does not allow at all:
 * `internal/portal`'s no-external-assets test greps the BUILT OUTPUT for
 * their hosts, because the rule is "nothing off-origin is referenced", not
 * "nothing off-origin succeeds". Each replacement keeps the code valid:
 *
 *   - The logos of the "API docs by Redocly" badge, on `cdn.redoc.ly`. An
 *     empty `data:` URI fails to decode, and Redoc's own `onError` then
 *     drops the <img>. The badge's link and text stay — the attribution is
 *     not the target, the request is.
 *   - Redoc 3's icon sets, on `cdn.redocly.com`, fetched only for an icon a
 *     document names by `font-awesome`/`code` set — this one names none.
 *     `data:,` makes any such icon a broken image instead of a request.
 *   - Redoc 3's telemetry collector, `otel.cloud.redocly.com`. The empty
 *     string is the value its tracer reads as "no collector configured", so
 *     it never starts — a second lock behind `disableTelemetry` in
 *     apiDocs.ts, which this does not replace.
 *   - The search index's worker probe. Redoc tries `Function(source)` to
 *     decide whether to index in a worker, and falls back to indexing on the
 *     page when that throws — which, under `script-src 'self'`, it always
 *     does, after the browser has logged the `eval` violation. Answering
 *     "no worker" up front gives the same in-page search without the attempt.
 *     (Redoc 2 built its index in a `blob:` worker with no fallback, which is
 *     why search was switched off then; in Redoc 3 it works.)
 *   - The `TreeViewIcons` @font-face, inlined as a `data:` font, which
 *     `font-src 'self'` refuses. It draws file-tree icons in a Markdown
 *     code-block feature this document does not use; `local()` names a font
 *     no machine has, so nothing is fetched and nothing is refused.
 *
 * A pattern that stops matching fails the build rather than silently
 * passing: an upgrade that changes one has to be looked at.
 */
const REDOC_REWRITES: readonly { name: string; pattern: RegExp; replacement: string }[] = [
  {
    name: 'badge logo (full)',
    pattern: /https:\/\/cdn\.redoc\.ly\/redoc\/logo-mini-full\.svg/g,
    replacement: 'data:,',
  },
  {
    name: 'badge logo',
    pattern: /https:\/\/cdn\.redoc\.ly\/redoc\/logo-mini\.svg/g,
    replacement: 'data:,',
  },
  {
    name: 'icon set (font-awesome)',
    pattern: /https:\/\/cdn\.redocly\.com\/icons\/fa\/[0-9.]+/g,
    replacement: 'data:,',
  },
  {
    name: 'icon set (code)',
    pattern: /https:\/\/cdn\.redocly\.com\/icons\/code\/[0-9.]+/g,
    replacement: 'data:,',
  },
  {
    name: 'telemetry collector',
    pattern: /https:\/\/otel\.cloud\.redocly\.com\/v1\/traces/g,
    replacement: '',
  },
  {
    name: 'search worker probe',
    pattern: /try\{return [\w$]+\._factory\?\?=Function\([\w$]+\),!0\}catch\{return!1\}/g,
    replacement: 'return!1',
  },
  {
    name: 'TreeViewIcons data: font',
    pattern: /url\('data:application\/font-woff;base64,[A-Za-z0-9+/=]+'\) format\('woff'\)/g,
    replacement: "local('gawk-no-such-font')",
  },
];

/**
 * What must not survive anywhere in the output, JS or CSS, whichever package
 * it came from. Wider than the rewrites on purpose: they match the one shape
 * Redoc ships today, these the shapes the same hazard takes elsewhere — a
 * `??=` lowered by a stricter target, `new Function`, the legacy font MIME
 * types, a quote escaped inside a JS string.
 */
const CSP_LEFTOVERS: readonly [hazard: string, pattern: RegExp][] = [
  [
    'a search worker probe (`_factory ??= Function(…)`), which script-src refuses',
    /_factory\s*(?:\?\?=|\?\?\s*\([^)]*=)\s*(?:new\s+)?Function\(/,
  ],
  [
    // `octet-stream` is a font MIME type for some tools and an image's for
    // others; either way the CSP refuses it, so the label says data: URL.
    'an inline data: URL (a font or opaque binary) that the CSP refuses',
    /url\(\s*\\?['"]?\s*data:(?:font\/|application\/(?:x-)?font|application\/vnd\.ms-fontobject|application\/octet-stream)/i,
  ],
];

function rewriteRedocForCsp() {
  const seen = new Set<string>();
  return {
    name: 'gawk:rewrite-redoc-for-csp',
    apply: 'build' as const,
    transform(code: string, id: string) {
      if (!id.includes('/node_modules/redoc/')) return null;
      let out = code;
      for (const { name, pattern, replacement } of REDOC_REWRITES) {
        const next = out.replace(pattern, replacement);
        if (next !== out) seen.add(name);
        out = next;
      }
      return out === code ? null : { code: out, map: null };
    },
    buildEnd(error?: Error) {
      // A build that already failed reports its own error, not this one.
      if (error) return;
      const missing = REDOC_REWRITES.map((r) => r.name).filter((name) => !seen.has(name));
      if (missing.length > 0) {
        throw new Error(
          `Redoc no longer matches: ${missing.join(', ')}. Re-check what it would make the CSP refuse (vite.config.ts).`,
        );
      }
    },
    // `seen` proves each rewrite matched ONCE, not that nothing of its kind
    // is left. The off-origin hosts have a second net (internal/portal's
    // no-external-assets test); these two CSP hazards do not, so every
    // emitted chunk AND stylesheet is checked for any survivor — a portal
    // @font-face small enough for Vite to inline lands in app.css, not JS.
    generateBundle(
      _options: unknown,
      bundle: Record<string, { type: string; code?: string; source?: string | Uint8Array }>,
    ) {
      for (const [file, out] of Object.entries(bundle)) {
        // A stylesheet handed over as bytes is decoded, not skipped: a net that
        // quietly stops covering CSS is the failure this check exists for.
        const text =
          out.type === 'chunk'
            ? out.code
            : !file.endsWith('.css')
              ? undefined
              : typeof out.source === 'string'
                ? out.source
                : out.source instanceof Uint8Array
                  ? new TextDecoder().decode(out.source)
                  : undefined;
        if (!text) continue;
        for (const [hazard, pattern] of CSP_LEFTOVERS) {
          if (pattern.test(text)) {
            throw new Error(`${file} contains ${hazard} (vite.config.ts, the CSP leftover check).`);
          }
        }
      }
    },
  };
}

export default defineConfig({
  plugins: [react(), rewriteRedocForCsp()],
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
    // `@gawk/oidc-session` is a `file:` link to ../../common-ts (docs/55 D3),
    // and Vite resolves it to that real path — outside this project, where the
    // dev server (and vitest, which runs the package's own tests from here)
    // refuses to read by default.
    fs: { allow: ['.', '../../common-ts'] },
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
  //
  // The shared session package (docs/55 D3) is counted too: it is bundled into
  // this SPA exactly as it was when it lived in src/auth, so leaving it out
  // would move the badge without any code having changed. `allowExternal` is
  // what lets v8 count a file outside this project; its harness (testing.ts)
  // is test code and excluded like the tests.
  test: {
    // Node 25+ ships its own Web Storage globals, enabled by default. Without
    // --localstorage-file its `localStorage` is undefined, and it shadows the
    // jsdom one, so every storage-backed test fails with "Cannot read
    // properties of undefined (reading 'clear')". Turn it off in the test
    // workers so jsdom's implementation is the one in scope. Harmless on
    // older Node, which accepts the flag.
    execArgv: ['--no-experimental-webstorage'],
    // The shared session package (docs/55 D3) is tested where it lives, by
    // this run: it has no toolchain of its own. Every consumer's vitest
    // includes it, so a change to it is exercised against each of them.
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
