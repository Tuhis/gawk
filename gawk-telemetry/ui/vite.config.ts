import { defineConfig } from 'vite';
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
//    built output.
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
  server: {
    proxy: Object.fromEntries(
      ['/live', '/v1', '/mcp'].map((path) => [
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
});
