// Licensing against the real server (docs/architecture/licensing.md). The
// shared node runs with the suite's test key and every feature on; a second
// node without a key shows the community license: the license page, the
// pages that ask for a key instead of their content (with no failed request
// behind them), and the API's 402 for a third policy.
import { request as pwRequest } from '@playwright/test';
import { env, expect, main, settle, SIGNED_OUT_401, test } from './fixtures';
import { ADMIN_USER, startServer, type Server } from './support/kudurru';

test('the license page shows the test key, its limits and every feature as included', async ({ page }) => {
  await page.goto('/license');
  await settle(page);
  await expect(main(page).getByRole('heading', { name: 'License', level: 1 })).toBeVisible();
  await expect(main(page).getByText('Enterprise', { exact: true })).toBeVisible();
  await expect(main(page).getByText('lic_e2e_test')).toBeVisible();
  await expect(page.getByTestId('license-nodes')).toHaveText('1 of 10');
  for (const f of ['rbac', 'oidc', 'audit', 'longterm', 'requests', 'edge', 'integrations']) {
    await expect(page.getByTestId(`feature-${f}`)).toContainText('Included');
  }
  // A healthy key shows no banner above the pages.
  await expect(page.getByTestId('license-banner')).toHaveCount(0);
});

test('the licensed pages show their content', async ({ page }) => {
  for (const [path, heading] of [
    ['/history', 'Long-term'],
    ['/requests', 'Requests'],
  ] as const) {
    await page.goto(path);
    await settle(page);
    await expect(main(page).getByRole('heading', { name: heading, level: 1 })).toBeVisible();
    await expect(page.getByTestId(/^locked-/)).toHaveCount(0);
  }
  await page.goto('/access?tab=audit');
  await settle(page);
  await expect(page.getByTestId('locked-audit')).toHaveCount(0);
});

test.describe('a node without a key', () => {
  let server: Server;
  const password = `community-${Date.now()}-password`;
  // The sign-in page asks who is signed in and gets a 401; a 402 would still fail the test.
  test.use({ storageState: { cookies: [], origins: [] }, allowConsole: SIGNED_OUT_401 });

  test.beforeAll(async () => {
    server = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
      upstream: env('KUDURRU_E2E_UPSTREAM'),
      node: 'e2e-community',
      adminPassword: password,
      community: true,
    });
  });
  test.afterAll(async () => {
    await server?.stop();
  });

  test('shows the community license and asks for a key where one is needed', async ({ page }) => {
    await page.goto(server.url);
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();
    await settle(page);
    // Running without a key is supported: no banner, no failed request.
    await expect(page.getByTestId('license-banner')).toHaveCount(0);

    await page.getByRole('navigation', { name: 'Side navigation' }).getByRole('link', { name: 'License', exact: true }).click();
    await expect(main(page).getByRole('heading', { name: 'License', level: 1 })).toBeVisible();
    await expect(main(page).getByText('No key', { exact: true }).first()).toBeVisible();
    await expect(page.getByTestId('license-nodes')).toHaveText('1 of 1');
    for (const f of ['rbac', 'oidc', 'audit', 'longterm', 'requests', 'edge', 'integrations']) {
      await expect(page.getByTestId(`feature-${f}`)).toContainText('Needs a key');
    }

    for (const [path, feature] of [
      ['/history', 'longterm'],
      ['/requests', 'requests'],
      ['/access?tab=audit', 'audit'],
    ] as const) {
      await page.goto(`${server.url}${path}`);
      await settle(page);
      await expect(page.getByTestId(`locked-${feature}`)).toContainText('needs a license key');
    }
    // The block page itself stays free; only the request form is gone. The
    // server and the pages above made no request that failed with a 402
    // (the suite fails on console errors).
  });

  test('refuses a third policy with 402 and keeps the two that exist', async () => {
    const api = await pwRequest.newContext({ baseURL: server.url, ignoreHTTPSErrors: true });
    const login = await api.post('/api/v1/auth/login', { data: { username: ADMIN_USER, password } });
    expect(login.ok()).toBe(true);
    const csrf = ((await login.json()) as { csrf: string }).csrf;
    const doc = (name: string) => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name }, spec: { clientGroupRefs: ['default'] } });
    const put = (name: string) => api.put(`/api/v1/objects/policies/default/${name}`, { data: doc(name), headers: { 'X-Kudurru-CSRF': csrf } });

    // default + e2e-blocking already exist, so the free limit of two is reached.
    const refused = await put('third');
    expect(refused.status()).toBe(402);
    expect(((await refused.json()) as { error: { key: string } }).error.key).toBe('license_required');
    // Editing an existing policy is not creating one.
    expect((await put('e2e-blocking')).status()).not.toBe(402);

    const info = await api.get('/api/v1/license');
    expect(await info.json()).toMatchObject({ state: 'community', nodes: 1, policyLimit: 2, features: [] });
    await api.dispose();
  });
});
