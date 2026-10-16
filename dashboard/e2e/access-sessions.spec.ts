// Actual SQLite, two browser sessions, no shared account or global-setup state.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';
import { env, expect, main, test } from './fixtures';
import { startUpstream } from './support/dns';
import { ADMIN_USER, startServer, type Server } from './support/kudurru';

process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1';
// Override the shared env-based fixture before beforeAll. Every request below
// uses the actual isolated node's absolute URL, never this inert base URL.
test.use({ baseURL: 'http://127.0.0.1', storageState: { cookies: [], origins: [] }, trace: 'off', screenshot: 'off', video: 'off', allowConsole: /status of 401|status of 503.*\/api\/v1\/auth\/(?:sessions|status)\)/ });

async function privateFill(field: Locator, value: string) {
  try { await field.fill(value); } catch { throw new Error('Private input could not be filled'); }
}

async function login(page: Page, node: Server) {
  await page.goto(node.url);
  await expect.poll(async () => {
    const response = await page.request.get(`${node.url}/api/v1/auth/status`);
    const ready = response.ok();
    await response.dispose();
    return ready;
  }, { timeout: 15_000 }).toBe(true);
  await privateFill(page.getByRole('textbox', { name: 'Username' }), ADMIN_USER);
  await privateFill(page.getByLabel(/^Password/), node.adminPassword);
  const submitted = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/auth/login');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  const response = await submitted;
  // Login and revoke are single-shot. A readiness read never reserves admission.
  expect(response.status(), 'single login attempt').toBe(200);
  await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
}

async function refreshSessions(page: Page) {
  const refresh = page.getByRole('button', { name: 'Refresh sessions', exact: true });
  const ready = await refresh.evaluateAll((buttons) => buttons.length === 1 && !(buttons[0] as HTMLButtonElement).disabled);
  if (ready) await refresh.click();
}

async function meStatus(page: Page, node: Server) {
  const response = await page.request.get(`${node.url}/api/v1/auth/me`);
  const status = response.status();
  await response.dispose();
  return status;
}

test.describe('Access own sessions on an isolated node', () => {
  let node: Server;
  let upstream: Awaited<ReturnType<typeof startUpstream>>;
  test.beforeAll(async () => {
    upstream = await startUpstream();
    // An explicit dedicated binary is required; never silently build/use bin/kudurru.
    node = await startServer(env('KUDURRU_BIN'), { upstream: upstream.address, node: 'e2e-own-sessions', adminPassword: randomBytes(24).toString('base64url') });
  });
  test.afterAll(async () => { await node?.stop(); await upstream?.close(); });

  test('revokes another session, retains this one, then revokes this session and clears local auth', async ({ page, browser }) => {
    test.setTimeout(90_000);
    await login(page, node);
    const second = await browser.newContext({ ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] }, serviceWorkers: 'block' });
    try {
      const otherPage = await second.newPage();
      await login(otherPage, node);
      await page.goto(`${node.url}/access`);
      const table = page.getByRole('table', { name: 'Your browser sessions' });
      await expect.poll(async () => {
        await refreshSessions(page);
        return table.getByRole('button', { name: /^Revoke session / }).count();
      }, { timeout: 15_000 }).toBe(1);
      await expect(table.getByText('Current session', { exact: true })).toHaveCount(1);
      const noncurrent = table.getByRole('button', { name: /^Revoke session / });
      await expect(noncurrent).toBeEnabled();
      await noncurrent.click();
      const revoked = page.waitForResponse((r) => r.request().method() === 'DELETE' && new URL(r.url()).pathname.startsWith('/api/v1/auth/sessions/'));
      await page.getByRole('dialog').getByRole('button', { name: 'Revoke selected session', exact: true }).click();
      expect((await revoked).status(), 'single noncurrent revoke').toBe(204);
      // Observe backend ownership independently of table rendering. Record ONLY
      // status codes, never a response body, cookie, CSRF, session ID or principal.
      await expect.poll(() => meStatus(page, node), { timeout: 15_000 }).toBe(200);
      await expect.poll(() => meStatus(otherPage, node), { timeout: 15_000 }).toBe(401);
      await expect.poll(() => meStatus(page, node), { timeout: 15_000 }).toBe(200);
      process.stdout.write('Noncurrent revoke proof: caller HTTP200, target HTTP401, caller HTTP200.\n');
      await expect.poll(async () => {
        const current = table.getByRole('button', { name: 'Revoke this session', exact: true });
        // One atomic boolean observation: a transient GET503 can remove the
        // table. Never park an inner locator wait while safe polling is needed.
        const ready = await current.evaluateAll((buttons) => buttons.length === 1 && !(buttons[0] as HTMLButtonElement).disabled);
        if (ready) return true;
        await refreshSessions(page);
        return false;
      }, { timeout: 15_000 }).toBe(true);
      await expect(main(page).getByRole('heading', { name: 'Access', exact: true })).toBeVisible();

      let logoutRequests = 0;
      page.on('request', (r) => { if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/auth/logout') logoutRequests++; });
      await table.getByRole('button', { name: 'Revoke this session', exact: true }).click();
      const selfRevoked = page.waitForResponse((r) => r.request().method() === 'DELETE' && new URL(r.url()).pathname.startsWith('/api/v1/auth/sessions/'));
      await page.getByRole('dialog').getByRole('button', { name: 'Revoke this session', exact: true }).click();
      expect((await selfRevoked).status(), 'single current revoke').toBe(204);
      await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
      expect(logoutRequests).toBe(0);
      await expect.poll(() => meStatus(page, node), { timeout: 15_000 }).toBe(401);
      await expect(page.getByRole('table', { name: 'Your browser sessions' })).toHaveCount(0);
    } finally { await second.close(); }
  });
});
