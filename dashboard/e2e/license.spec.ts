// Licensing against the real server (docs/architecture/licensing.md). The
// shared node runs with the suite's test key and every feature on; a second
// node without a key shows the community license: the license page, the
// pages that ask for a key instead of their content (with no failed request
// behind them), and the seeded default as its only policy: Policies says so
// and the API answers 402 to a second one.
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
    await expect(main(page).getByText('1 (the default policy)', { exact: true })).toBeVisible();
    for (const f of ['rbac', 'oidc', 'audit', 'longterm', 'requests', 'edge', 'integrations']) {
      await expect(page.getByTestId(`feature-${f}`)).toContainText('Needs a key');
    }

    // The seeded default is the one policy: "New policy" is disabled and Policies says why.
    await page.goto(`${server.url}/policies`);
    await settle(page);
    await expect(page.getByTestId('policy-limit')).toContainText('Without a valid license key only the default policy is available');
    await expect(main(page).getByRole('button', { name: 'New policy' })).toHaveAttribute('aria-disabled', 'true');

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

  test('keeps the seeded default as the only policy: a second one is refused with 402', async () => {
    const api = await pwRequest.newContext({ baseURL: server.url, ignoreHTTPSErrors: true });
    const login = await api.post('/api/v1/auth/login', { data: { username: ADMIN_USER, password } });
    expect(login.ok()).toBe(true);
    const csrf = ((await login.json()) as { csrf: string }).csrf;
    const doc = (name: string) => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name }, spec: { clientGroupRefs: ['default'] } });
    const put = (name: string) => api.put(`/api/v1/objects/policies/default/${name}`, { data: doc(name), headers: { 'X-Kudurru-CSRF': csrf } });
    const names = async () => ((await (await api.get('/api/v1/objects/policies')).json()) as { items: { metadata: { name: string } }[] }).items.map((p) => p.metadata.name);

    // The bootstrap seeded Policy "default", which fills the free limit of one.
    expect(await names()).toEqual(['default']);
    const refused = await put('second');
    expect(refused.status()).toBe(402);
    const body = (await refused.json()) as { error: { key: string; message: string } };
    expect(body.error.key).toBe('license_required');
    expect(body.error.message).toContain('only the policy default/default can be created');
    // Editing the default is not creating a policy.
    expect((await put('default')).status()).toBe(200);
    expect(await names()).toEqual(['default']);

    const info = await api.get('/api/v1/license');
    expect(await info.json()).toMatchObject({ state: 'community', nodes: 1, policyLimit: 1, features: [] });
    await api.dispose();
  });
});
