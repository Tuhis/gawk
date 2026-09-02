import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// R28 (docs/33 D15): the app version is the telemetry SCHEMA version — every
// sample is attributable to the exact release that produced it, with no
// separate field to keep in sync. Read from package.json at build time so the
// released version and the one clients report can never disagree.
const pkg = JSON.parse(
  readFileSync(fileURLToPath(new URL('./package.json', import.meta.url)), 'utf8'),
) as { version: string }

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  define: {
    __GAWK_APP_VERSION__: JSON.stringify(pkg.version),
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
})
