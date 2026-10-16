// Actual Enterprise license, SQLite publication, and isolated license withdrawal.
import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { Locator, Page } from '@playwright/test';
import { env, expect, main, test } from './fixtures';
import { startUpstream } from './support/dns';
import { ADMIN_USER, E2E_LICENSE_KEY, startServer, type Server } from './support/kudurru';

process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1';
test.use({ baseURL: 'http://127.0.0.1', storageState: { cookies: [], origins: [] }, trace: 'off', screenshot: 'off', video: 'off', allowConsole: /status of (401|503)|status of 402.*\/api\/v1\/unblockrequests\?state=Pending&limit=1\)/ });
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==', 'base64');
const DATA = `data:image/png;base64,${PNG.toString('base64')}`;
const quote = (s: string) => `'${s.replace(/'/g, `'"'"'`)}'`;
async function privateFill(field: Locator, value: string) { try { await field.fill(value); } catch { throw new Error('Private input could not be filled'); } }
async function login(page: Page, node: Server) {
  await page.goto(node.url);
  await expect.poll(async () => { const r = await page.request.get(`${node.url}/api/v1/auth/status`); const ok = r.ok(); await r.dispose(); return ok; }, { timeout: 20_000 }).toBe(true);
  await privateFill(page.getByRole('textbox', { name: 'Username' }), ADMIN_USER);
  await privateFill(page.getByLabel(/^Password/), node.adminPassword);
  const response = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/auth/login');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  expect((await response).status(), 'single credential POST').toBe(200);
  await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
}
async function publicInfo(page: Page, node: Server) {
  const r = await page.request.get(`${node.url}/api/v1/branding`);
  const result = r.ok() ? await r.json() as { enabled: boolean; name: string; logoLight: string; logoDark: string; favicon: string; accentColor: string; validUntil?: string } : null;
  await r.dispose(); return result;
}
async function edit(page: Page) {
  const button = page.getByRole('button', { name: 'Edit branding', exact: true });
  // A failed or old public capture defaults the UI until the next15s poll.
  // Opening this local editor is a read-only action, not a mutation retry.
  await expect(button).toBeEnabled({ timeout: 25_000 });
  await button.click();
}
async function save(page: Page) {
  const response = page.waitForResponse((r) => r.request().method() === 'PATCH' && new URL(r.url()).pathname === '/api/v1/settings');
  await page.getByRole('button', { name: 'Save branding', exact: true }).click();
  expect((await response).status(), 'single settings PATCH').toBe(200);
  await expect(page.getByRole('button', { name: 'Edit branding', exact: true })).toBeVisible();
}
test.describe('Enterprise branding on an isolated node', () => {
  let dir: string, licenseFile: string, node: Server, upstream: Awaited<ReturnType<typeof startUpstream>>;
  test.beforeAll(async () => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'kudurru-branding-ui-'));
    licenseFile = path.join(dir, 'license.key'); fs.copyFileSync(E2E_LICENSE_KEY, licenseFile); fs.chmodSync(licenseFile, 0o600);
    const launcher = path.join(dir, 'serve');
    // Select only the private copy; an inherited inline license cannot win.
    fs.writeFileSync(launcher, `#!/bin/sh\nunset KUDURRU_LICENSE_KEY\nexport KUDURRU_LICENSE_KEY_FILE=${quote(licenseFile)}\nexec ${quote(path.resolve(env('KUDURRU_BIN')))} "$@"\n`, { mode: 0o700 });
    upstream = await startUpstream();
    node = await startServer(launcher, { upstream: upstream.address, node: 'e2e-branding', adminPassword: randomBytes(24).toString('base64url') });
  });
  test.afterAll(async () => { await node?.stop(); await upstream?.close(); if (dir) fs.rmSync(dir, { recursive: true, force: true }); });
  test('saves/reset settings, brands anonymous and authenticated pages, then defaults on real license loss', async ({ page, browser }) => {
    test.setTimeout(180_000);
    await expect.poll(async () => { const b = await publicInfo(page, node); return !!(b?.enabled && b.name === 'kudurru' && !b.logoLight && !b.logoDark && !b.favicon && !b.accentColor && b.validUntil && Date.parse(b.validUntil) > Date.now()); }, { timeout: 20_000 }).toBe(true);
    await login(page, node);
    const originalIcons = await page.locator('head link[rel="icon"]').evaluateAll((nodes) => nodes.map((n) => ({ href: n.getAttribute('href'), sizes: n.getAttribute('sizes'), type: n.getAttribute('type') })));
    await page.goto(`${node.url}/settings`);
    await edit(page);
    await page.getByRole('textbox', { name: 'Organization name' }).fill('Example Network');
    await page.getByRole('textbox', { name: 'Accent color' }).fill('#0064E0');
    await page.getByLabel('Light logo', { exact: true }).and(page.locator('input[type="file"]')).setInputFiles({ name: 'organization.png', mimeType: 'image/png', buffer: PNG });
    await page.getByLabel('Favicon', { exact: true }).and(page.locator('input[type="file"]')).setInputFiles({ name: 'favicon.png', mimeType: 'image/png', buffer: PNG });
    await expect(page.getByRole('button', { name: 'Save branding' })).toBeEnabled();
    await save(page);
    await expect.poll(async () => { const b = await publicInfo(page, node); return !!(b?.enabled && b.name === 'Example Network' && b.logoLight === DATA && b.favicon === DATA && b.validUntil && Date.parse(b.validUntil) > Date.now()); }).toBe(true);
    await expect(page).toHaveTitle('Example Network', { timeout: 25_000 });
    await expect(page.getByRole('link', { name: 'Example Network dashboard home' })).toBeVisible();
    expect(await page.locator('head link[rel="icon"]').evaluateAll((nodes, data) => nodes.length === 1 && nodes[0]!.getAttribute('href') === data, DATA)).toBe(true);
    const anonymous = await browser.newContext({ ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] }, serviceWorkers: 'block' });
    try {
      const signedOut = await anonymous.newPage(); await signedOut.goto(node.url);
      await expect(signedOut.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
      await expect(signedOut).toHaveTitle('Example Network', { timeout: 25_000 });
      expect(await signedOut.getByRole('img', { name: 'Example Network' }).first().evaluate((img, data) => img.getAttribute('src') === data, DATA)).toBe(true);
      // Real server block-page rendering, without relaxing the iframe sandbox.
      const block = await page.request.get(`${node.blockPage}/`, { headers: { Host: 'ads.example.net' } });
      const html = await block.text(); expect(html.includes('Example Network')).toBe(true); await block.dispose();
      await edit(page);
      await page.getByRole('button', { name: 'Reset branding fields', exact: true }).click(); await save(page);
      await expect.poll(async () => { const b = await publicInfo(page, node); return !!(b?.enabled && b.name === 'kudurru' && !b.logoLight && !b.logoDark && !b.favicon && !b.accentColor && b.validUntil && Date.parse(b.validUntil) > Date.now()); }).toBe(true);
      await edit(page);
      await page.getByRole('textbox', { name: 'Organization name' }).fill('Retained Network');
      await page.getByRole('textbox', { name: 'Accent color' }).fill('#0064E0'); await save(page);
      await expect.poll(async () => (await publicInfo(page, node))?.name).toBe('Retained Network');
      // Remove this test's copy only. The production minute watcher performs loss.
      fs.unlinkSync(licenseFile);
      await expect.poll(async () => { const b = await publicInfo(page, node); return !!(b && !b.enabled && b.name === 'kudurru' && !b.logoLight && !b.logoDark && !b.favicon && !b.accentColor && !b.validUntil); }, { timeout: 95_000, intervals: [500, 1000, 2000] }).toBe(true);
      await expect(page).toHaveTitle('kudurru', { timeout: 25_000 });
      await expect(signedOut).toHaveTitle('kudurru', { timeout: 25_000 });
      await expect(signedOut.locator('head link[rel="icon"]')).toHaveCount(originalIcons.length);
      expect(await signedOut.locator('head link[rel="icon"]').evaluateAll((nodes) => nodes.map((n) => ({ href: n.getAttribute('href'), sizes: n.getAttribute('sizes'), type: n.getAttribute('type') })))).toEqual(originalIcons);
      expect(await signedOut.locator('head link[rel="icon"]').evaluateAll((nodes, data) => nodes.every((n) => n.getAttribute('href') !== data), DATA)).toBe(true);
      await page.reload();
      await expect(page.getByRole('button', { name: 'Edit branding', exact: true })).toBeDisabled();
      await expect(page.getByText('Organization: Retained Network', { exact: true })).toBeVisible();
      expect(await page.locator('html').getAttribute('data-astryx-theme')).toBe('kudurru');
    } finally { await anonymous.close(); }
  });
});
