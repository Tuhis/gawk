import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { defineConfig } from 'vite'
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
})
