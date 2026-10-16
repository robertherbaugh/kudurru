// Isolated real SQLite account; credential-bearing artifacts are never recorded.
import { createHmac, randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';
import { env, expect, main, test } from './fixtures';
import { ADMIN_USER, sleep, startServer, type Server } from './support/kudurru';

// Playwright also creates failure ARIA snapshots independently of screenshots.
// A provisioning key must never enter that diagnostic artifact.
process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1';

test.use({ storageState: { cookies: [], origins: [] }, trace: 'off', screenshot: 'off', video: 'off', allowConsole: /status of 401|status of 503.*\/api\/v1\/auth\/(?:totp|status|login)\)/ });

// RFC6238 test derivation only. The production dashboard performs no factor crypto.
function codeAt(secret: string): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const char of secret) {
    const value = alphabet.indexOf(char);
    if (value < 0) throw new Error('Invalid test provisioning profile');
    bits += value.toString(2).padStart(5, '0');
  }
  const bytes = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30_000)));
  const hash = createHmac('sha1', Buffer.from(bytes)).update(counter).digest();
  const offset = hash[hash.length - 1]! & 15;
  return ((hash.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).toString().padStart(6, '0');
}

/** Replace any Playwright input failure so a call log cannot echo private values. */
async function privateFill(field: Locator, value: string) {
  try { await field.fill(value); } catch { throw new Error('Private input could not be filled'); }
}

async function publicationReady(page: Page) {
  await expect.poll(async () => {
    const response = await page.request.get(new URL('/api/v1/auth/status', page.url()).href);
    const ready = response.ok();
    await response.dispose();
    return ready;
  }, { timeout: 15_000, intervals: [250, 500, 1000] }).toBe(true);
}

async function submitLogin(page: Page, passwordOnly = false) {
  const deadline = Date.now() + 30_000;
  for (let attempt = 0; ; attempt++) {
    const result = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/v1/auth/login');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    const response = await result;
    if (response.ok()) return;
    let pending = false;
    const headers = await response.allHeaders();
    const bytes = await response.body();
    if (bytes.length <= 4096) {
      try { pending = (JSON.parse(bytes.toString()) as { error?: { key?: unknown } }).error?.key === 'generation_pending'; } catch { /* No raw body diagnostics. */ }
    }
    // This exact refusal is before admission. Never repeat factor proof, a
    // handler response, cookies/redirects, or an uncertain transport outcome.
    if (!passwordOnly || response.status() !== 503 || !pending || headers['set-cookie'] || headers.location || attempt >= 19 || Date.now() >= deadline) {
      throw new Error(`Login attempt refused: HTTP ${response.status()}, pre-handler generation pending=${pending}`);
    }
    await sleep(Math.min(1000, deadline - Date.now()));
  }
}

async function factorStatus(page: Page, enabled: boolean) {
  // Only safe GETs may be repeated during publisher warmup. No proof is replayed.
  await expect.poll(async () => {
    const refresh = page.getByRole('button', { name: 'Refresh authenticator status', exact: true });
    if (await refresh.isVisible()) await refresh.click();
    return page.getByText(enabled ? 'An authenticator is enabled.' : 'No authenticator is enabled.', { exact: true }).isVisible();
  }, { timeout: 15_000, intervals: [250, 500, 1000] }).toBe(true);
}

async function nextStep() { await sleep(30_000 - Date.now() % 30_000 + 1000); }

test.describe('Access authenticator on an isolated node', () => {
  let server: Server;
  test.beforeAll(async () => {
    server = await startServer(env('KUDURRU_E2E_BIN_PATH'), { upstream: env('KUDURRU_E2E_UPSTREAM'), node: 'e2e-authenticator', adminPassword: randomBytes(24).toString('base64url') });
    process.env.KUDURRU_E2E_URL = server.url;
  });
  test.afterAll(async () => { await server?.stop(); });

  test('enrolls a manual key, signs in with a fresh code, and recovers the exact self UID', async ({ page }) => {
    test.setTimeout(120_000);
    await page.goto(server.url);
    await publicationReady(page);
    await privateFill(page.getByRole('textbox', { name: 'Username' }), ADMIN_USER);
    await privateFill(page.getByLabel(/^Password/), server.adminPassword);
    await submitLogin(page, true);
    await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
    await page.goto(`${server.url}/access`);
    await factorStatus(page, false);
    await page.getByRole('button', { name: 'Set up authenticator', exact: true }).click();
    await privateFill(page.getByLabel('Current password', { exact: true }), server.adminPassword);
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    const keyField = page.getByLabel('Authenticator key', { exact: true });
    await expect(keyField).toBeVisible();
    let secret = await keyField.inputValue();
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await privateFill(page.getByLabel('Current password again', { exact: true }), server.adminPassword);
    await privateFill(page.getByLabel('New authenticator code', { exact: true }), codeAt(secret));
    await page.getByRole('button', { name: 'Confirm authenticator', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await nextStep();
    await publicationReady(page);
    await privateFill(page.getByRole('textbox', { name: 'Username' }), ADMIN_USER);
    await privateFill(page.getByLabel(/^Password/), server.adminPassword);
    await privateFill(page.getByLabel(/Authenticator code/), codeAt(secret));
    // Drop the private test seed before continuing with recovery.
    // eslint-disable-next-line no-useless-assignment
    secret = '';
    await submitLogin(page);
    await expect(main(page).getByRole('heading', { name: 'Access', exact: true })).toBeVisible();
    await factorStatus(page, true);
    await page.getByRole('button', { name: 'Identity', exact: true }).click();
    await page.getByRole('button', { name: 'Recover authenticator for admin', exact: true }).click();
    await expect(page.getByText('The selected UID matches the freshly loaded inventory.', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Recover authenticator', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await publicationReady(page);
    await privateFill(page.getByRole('textbox', { name: 'Username' }), ADMIN_USER);
    await privateFill(page.getByLabel(/^Password/), server.adminPassword);
    await submitLogin(page, true);
    await expect(main(page).getByRole('heading', { name: 'Access', exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'You & tokens', exact: true }).click();
    await factorStatus(page, false);
  });
});
