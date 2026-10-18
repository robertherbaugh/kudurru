/// <reference types="vitest/config" />
// Build, dev server and test configuration for the kudurru dashboard.
//
// - `base: './'` emits relative asset URLs. index.html carries
//   `<base href="/">`, which the Go handler (web.Handler) rewrites to the
//   configured path prefix, so one bundle serves under any prefix (§10
//   "path-prefix aware").
// - No inline scripts or styles end up in index.html (strict CSP,
//   `default-src 'self'`): the module preload polyfill is off and every
//   style is an emitted .css file.
// - public/ (favicon, app icons, web manifest) is copied to the dist root
//   under stable names, which web.Handler serves like any other file.
// - `--mode mock` (bun run dev:mock) serves the MSW worker from mock-public/
//   instead and answers /api/v1 from src/mocks; production builds never
//   contain it (the mock dev server just goes without the tab icon).
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => ({
  base: './',
  plugins: [react()],
  publicDir: mode === 'mock' ? 'mock-public' : 'public',
  define: {
    'import.meta.env.KUDURRU_MOCK': JSON.stringify(mode === 'mock'),
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2023',
    sourcemap: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    chunkSizeWarningLimit: 900,
    rolldownOptions: {
      output: {
        // Stable, recognisable vendor chunks (long-lived cache across app
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
    port: 5173,
    proxy:
      mode === 'mock'
        ? undefined
        : {
            '/api': {
              target: process.env.KUDURRU_API ?? 'http://127.0.0.1:8080',
              changeOrigin: false,
            },
          },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
    include: ['src/**/*.test.{ts,tsx}'],
    testTimeout: 20000,
    pool: 'threads',
  },
}));
