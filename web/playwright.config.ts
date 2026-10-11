// Playwright end-to-end suite: the dashboard against a real `kudurru serve`
// with the embedded bundle (e2e/global-setup.ts starts it). `make web-e2e`
// builds the bundle and the binary and runs it; `bun run e2e` reuses
// ../bin/kudurru (or KUDURRU_BIN).
//
// Browser: Playwright's chromium in CI (`bunx playwright install chromium`);
// locally the system Chrome at /usr/bin/google-chrome when present, or the
// executable named by KUDURRU_E2E_CHROME.
import { defineConfig, devices } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ci = !!process.env.CI;
const systemChrome = '/usr/bin/google-chrome';
const chrome = process.env.KUDURRU_E2E_CHROME ?? (!ci && fs.existsSync(systemChrome) ? systemChrome : undefined);
const dir = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  globalSetup: './e2e/global-setup.ts',
  fullyParallel: true,
  forbidOnly: ci,
  retries: 0,
  workers: 4,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: ci ? [['list'], ['github'], ['html', { open: 'never' }]] : [['list'], ['html', { open: 'never' }]],
  use: {
    ...devices['Desktop Chrome'],
    // The API listener serves a self-signed certificate (listeners.api.tls.auto).
    ignoreHTTPSErrors: true,
    storageState: path.join(dir, 'e2e', '.auth', 'admin.json'),
    viewport: { width: 1280, height: 800 },
    actionTimeout: 10_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    ...(chrome ? { launchOptions: { executablePath: chrome } } : {}),
  },
  projects: [{ name: 'chromium' }],
});
