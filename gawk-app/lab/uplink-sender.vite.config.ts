// Build config for the uplink lab's browser sender (docs/75 OD5,
// tools/uplinklab/browser). Separate from vite.config.ts so the app bundle
// is untouched; the output lands under dist/, which .gitignore covers.
import { defineConfig } from 'vite';

export default defineConfig({
  base: './',
  publicDir: false,
  build: {
    outDir: process.env.UPLINK_SENDER_OUT ?? 'dist/uplink-sender',
    emptyOutDir: true,
    rollupOptions: { input: 'lab/uplink-sender.html' },
  },
});
