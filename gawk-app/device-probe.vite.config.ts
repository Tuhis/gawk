// Build config for the throwaway R22 device probe (device-probe.html).
// Separate from vite.config.ts so the app bundle is untouched.
import { defineConfig } from 'vite';

export default defineConfig({
  base: './',
  build: {
    // Under dist/, which .gitignore already covers — a probe build must not
    // leave an untracked directory behind.
    outDir: process.env.PROBE_OUT ?? 'dist/device-probe',
    emptyOutDir: true,
    target: 'safari16',
    rollupOptions: { input: 'device-probe.html' },
  },
});
