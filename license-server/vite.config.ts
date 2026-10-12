/// <reference types="vitest/config" />
// Build, dev server and test configuration of the license portal UI,
// adapted from web/vite.config.ts.
//
// - The Vite root is ui/ (ui/index.html); the build lands in dist/ui, which
//   the Bun server serves from a manifest of the built files
//   (server/http/static.ts), never by joining request paths onto the disk.
// - `base: './'` emits relative asset URLs; ui/index.html carries
//   `<base href="/">`, so deep links such as /licenses/lic_x load the same
//   bundle.
// - No inline scripts or styles end up in index.html (strict CSP,
//   `script-src 'self'`): the module preload polyfill is off and nothing is
//   inlined as a data: URL.
// - `bun run dev:ui` serves on 5174 and proxies /api to the Bun server on
//   8790 (`bun run dev:server`).
// - vitest runs the UI tests only (ui/src/**/*.test.{ts,tsx}) in jsdom; the
//   server's tests are bun tests under server/ (bunfig.toml).
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const here = fileURLToPath(new URL('.', import.meta.url));

export default defineConfig({
  root: 'ui',
  base: './',
  plugins: [react()],
  resolve: {
    // The wire types and the catalog shared with the server (ui/tsconfig.json paths).
    alias: { '@shared': fileURLToPath(new URL('./shared', import.meta.url)) },
  },
  build: {
    outDir: '../dist/ui',
    emptyOutDir: true,
    target: 'es2023',
    sourcemap: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    chunkSizeWarningLimit: 900,
    rolldownOptions: {
      output: {
        // Stable, recognisable vendor chunks (long-lived cache across
        // releases); everything else splits by route.
        codeSplitting: {
          groups: [
            { name: 'react', test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/ },
            { name: 'tanstack-query', test: /node_modules[\\/]@tanstack[\\/](query-core|react-query)[\\/]/ },
          ],
        },
      },
    },
  },
  server: {
    port: 5174,
    strictPort: true,
    proxy: {
      '/api': {
        target: process.env.LICENSE_SERVER_API ?? 'http://127.0.0.1:8790',
        changeOrigin: false,
      },
    },
  },
  test: {
    root: here,
    environment: 'jsdom',
    globals: true,
    setupFiles: ['ui/src/test/setup.ts'],
    css: false,
    include: ['ui/src/**/*.test.{ts,tsx}'],
    testTimeout: 20000,
    pool: 'threads',
  },
});
