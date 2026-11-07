// Exercise the embedded assets under the real Go CSP, including a path prefix.
import { randomBytes } from 'node:crypto';
import type { Page } from '@playwright/test';
import { env, expect, main, settle, test } from './fixtures';
import { startServer } from './support/kudurru';

test.use({ trace: 'off', screenshot: 'off', video: 'off' });

async function loadFaces(page: Page) {
  return page.evaluate(async () => {
    const results = [];
    for (const family of ['Inter', 'JetBrains Mono']) {
      for (const style of ['normal', 'italic']) {
        for (const weight of [400, 500, 600, 700]) {
          const faces = await document.fonts.load(`${style} ${weight} 14px "${family}"`, 'DNS example.test 192.0.2.1');
          results.push(faces.length > 0 && faces.every((face) => face.status === 'loaded'));
        }
      }
    }
    return results;
  });
}

async function sameOriginOnly(page: Page, origin: string) {
  const external: string[] = [];
  await page.route('**/*', async (route) => {
    if (new URL(route.request().url()).origin === origin) await route.continue();
    else {
      external.push(route.request().resourceType());
      await route.abort();
    }
  });
  return external;
}

test('Kudurru loads all font weights locally under the production CSP', async ({ page }) => {
  const external = await sameOriginOnly(page, new URL(env('KUDURRU_E2E_URL')).origin);
  const fonts: { path: string; status: number }[] = [];
  page.on('response', (response) => {
    if (response.request().resourceType() === 'font') fonts.push({ path: new URL(response.url()).pathname, status: response.status() });
  });
  const response = await page.goto('/');
  expect(response?.headers()['content-security-policy']).toContain("font-src 'self'");
  await settle(page);
  expect(await loadFaces(page)).toEqual(Array<boolean>(16).fill(true));
  expect(fonts).toHaveLength(4);
  expect(fonts.every((font) => font.path.startsWith('/assets/') && font.status === 200)).toBe(true);
  expect(external).toEqual([]);
  // Check the font actually used to paint the heading, not only its CSS stack.
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('DOM.enable');
    await cdp.send('CSS.enable');
    const { root } = await cdp.send('DOM.getDocument');
    const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: root.nodeId, selector: '#astryx-app-shell-main h1' });
    const { fonts: painted } = await cdp.send('CSS.getPlatformFontsForNode', { nodeId });
    expect(painted.some((font) => font.isCustomFont && font.familyName.startsWith('Inter') && font.glyphCount > 0)).toBe(true);
  } finally { await cdp.detach(); }
  const licenses = await page.request.get('/third-party-licenses.txt');
  expect(licenses.ok()).toBe(true);
  const text = await licenses.text();
  expect(text).toContain('@fontsource-variable/inter 5.3.0 (OFL-1.1)');
  expect(text).toContain('@fontsource-variable/jetbrains-mono 5.3.0 (OFL-1.1)');
  expect(text).toContain('SIL OPEN FONT LICENSE Version 1.1');
  await licenses.dispose();
});

test('font assets remain local under a nested dashboard prefix', async ({ browser }) => {
  const prefix = '/admin/dns';
  const server = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
    upstream: env('KUDURRU_E2E_UPSTREAM'), node: 'e2e-font-prefix', pathPrefix: prefix,
    adminPassword: randomBytes(24).toString('base64url'),
  });
  const context = await browser.newContext({ ignoreHTTPSErrors: true, serviceWorkers: 'block' });
  try {
    const page = await context.newPage();
    const external = await sameOriginOnly(page, new URL(server.url).origin);
    const fonts: string[] = [];
    page.on('response', (response) => {
      if (response.request().resourceType() === 'font' && response.ok()) fonts.push(new URL(response.url()).pathname);
    });
    const response = await page.goto(`${server.url}/`);
    expect(response?.headers()['content-security-policy']).toContain("font-src 'self'");
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    expect(await loadFaces(page)).toEqual(Array<boolean>(16).fill(true));
    expect(fonts).toHaveLength(4);
    expect(fonts.every((font) => font.startsWith(`${prefix}/assets/`))).toBe(true);
    expect(external).toEqual([]);
  } finally { await context.close(); await server.stop(); }
});

test.describe('font download failure', () => {
  test.use({ allowConsole: /Failed to load resource: net::ERR_FAILED/ });
  test('keeps visible system fallback text and usable navigation', async ({ page }) => {
    await page.route('**/*.woff2', (route) => route.abort('failed'));
    await page.goto('/');
    await settle(page);
    await page.evaluate(() => document.fonts.ready);
    await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
    expect(await page.evaluate(() => getComputedStyle(document.body).fontFamily)).toContain('system-ui');
    expect(await page.evaluate(() => [...document.fonts].some((face) => face.family === 'Inter' && face.status === 'error'))).toBe(true);
    await page.getByRole('navigation', { name: 'Side navigation' }).getByRole('link', { name: 'Zones', exact: true }).click();
    await settle(page);
    await expect(main(page).getByRole('heading', { name: 'Zones', exact: true })).toBeVisible();
  });
});
