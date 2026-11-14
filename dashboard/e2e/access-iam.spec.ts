// The IAM writers of the Identity tab against a real SQLite node of its own
// (D396): create a local account in the dashboard, sign in as it in a second
// browser (it must change its first password), disable it (its session ends
// and sign-in is refused), delete it; create a service account, issue a
// token in the dashboard and use it once; edit and delete an API-owned role
// binding (its labels and operator annotations survive the edit, and a
// binding of a scope kind the form does not offer is not editable). Only
// status codes are recorded:
// never a password, a token, a cookie or a CSRF value.
import { randomBytes } from 'node:crypto';
import type { Locator, Page } from '@playwright/test';
import { env, expect, main, test } from './fixtures';
import { ADMIN_USER, startServer, type Server } from './support/kudurru';

// A refused sign-in, the signed-out reads of the second browser and the
// 401s of the ended session are the point of the test.
test.use({ storageState: { cookies: [], origins: [] }, allowConsole: /status of (401|403)/ });

async function privateFill(field: Locator, value: string) {
  try {
    await field.fill(value);
  } catch {
    throw new Error('Private input could not be filled');
  }
}

async function signIn(page: Page, node: Server, user: string, password: string): Promise<number> {
  await page.goto(node.url);
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await privateFill(page.getByRole('textbox', { name: 'Username' }), user);
  await privateFill(page.getByLabel(/^Password/), password);
  const submitted = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/auth/login');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  return (await submitted).status();
}

async function meStatus(page: Page, node: Server): Promise<number> {
  const r = await page.request.get(`${node.url}/api/v1/auth/me`);
  const s = r.status();
  await r.dispose();
  return s;
}

/** Waits until a row action of the table is enabled (the list is current) and clicks it. */
async function rowAction(page: Page, table: string, name: string) {
  const button = main(page).getByRole('table', { name: table }).getByRole('button', { name, exact: true });
  await expect(button).toBeEnabled({ timeout: 15_000 });
  await button.click();
}

/** Submits the open dialog with its primary button and returns the one answer of `method path`. */
async function submitOnce(page: Page, button: string, method: string, path: string): Promise<number> {
  const answered = page.waitForResponse((r) => r.request().method() === method && new URL(r.url()).pathname === path);
  await page.getByRole('dialog').getByRole('button', { name: button, exact: true }).click();
  return (await answered).status();
}

test.describe('IAM writers on an isolated node', () => {
  let node: Server;
  const adminPassword = randomBytes(24).toString('base64url');
  test.beforeAll(async () => {
    node = await startServer(env('KUDURRU_E2E_BIN_PATH'), { upstream: env('KUDURRU_E2E_UPSTREAM'), node: 'e2e-iam', adminPassword });
  });
  test.afterAll(async () => {
    await node?.stop();
  });

  test('creates a local account, signs in as it, disables it and deletes it', async ({ page, browser }) => {
    test.setTimeout(120_000);
    const first = `first-${randomBytes(9).toString('base64url')}`;
    const changed = `changed-${randomBytes(9).toString('base64url')}`;
    expect(await signIn(page, node, ADMIN_USER, adminPassword), 'admin sign-in').toBe(200);
    await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
    await page.goto(`${node.url}/access?tab=iam`);

    // Create.
    const create = main(page).getByRole('button', { name: 'New user', exact: true });
    await expect(create).toBeEnabled({ timeout: 15_000 });
    await create.click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel(/^Username/).fill('e2e-alice');
    await dialog.getByLabel(/^Display name/).fill('Alice E2E');
    await privateFill(dialog.getByLabel(/^First password/), first);
    await privateFill(dialog.getByLabel(/^Confirm password/), first);
    expect(await submitOnce(page, 'Create user', 'POST', '/api/v1/iam/users'), 'single create').toBe(201);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    const users = main(page).getByRole('table', { name: 'Local users' });
    await expect(users.getByText('Alice E2E')).toBeVisible({ timeout: 15_000 });

    // Sign in as it in a second browser; the first password must be changed.
    const second = await browser.newContext({ ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] }, serviceWorkers: 'block' });
    try {
      const alice = await second.newPage();
      expect(await signIn(alice, node, 'e2e-alice', first), 'alice sign-in').toBe(200);
      const forced = alice.getByRole('alertdialog', { name: 'Choose a new password' });
      await expect(forced).toBeVisible();
      await privateFill(forced.getByRole('textbox', { name: /current password/i }), first);
      await privateFill(forced.getByRole('textbox', { name: /^new password/i }), changed);
      await privateFill(forced.getByRole('textbox', { name: /confirm/i }), changed);
      const pw = alice.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/auth/password');
      await forced.getByRole('button', { name: 'Change password', exact: true }).click();
      expect((await pw).status(), 'single password change').toBe(200);
      await expect(main(alice).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
      expect(await meStatus(alice, node)).toBe(200);

      // Disable: the account's session ends in the same commit.
      await page.reload();
      await rowAction(page, 'Local users', 'Disable e2e-alice');
      await expect(page.getByRole('dialog').getByText('Permanent revocation record').first()).toBeVisible();
      expect(await submitOnce(page, 'Disable account', 'PATCH', '/api/v1/iam/users/e2e-alice'), 'single disable').toBe(200);
      await expect(page.getByRole('dialog')).toHaveCount(0);
      await expect.poll(() => meStatus(alice, node), { timeout: 15_000 }).toBe(401);
      await expect(users.getByRole('row').filter({ hasText: 'Alice E2E' }).getByText('disabled', { exact: true })).toBeVisible({ timeout: 15_000 });
      expect(await signIn(alice, node, 'e2e-alice', changed), 'disabled sign-in refused').toBe(401);

      // Delete by the reviewed row.
      await rowAction(page, 'Local users', 'Delete e2e-alice');
      expect(await submitOnce(page, 'Delete account', 'DELETE', '/api/v1/iam/users/e2e-alice'), 'single delete').toBe(204);
      await expect(page.getByRole('dialog')).toHaveCount(0);
      await expect(users.getByText('Alice E2E')).toHaveCount(0, { timeout: 15_000 });
      expect(await signIn(alice, node, 'e2e-alice', changed), 'deleted sign-in refused').toBe(401);
    } finally {
      await second.close();
    }
  });

  test('creates a service account and uses a token issued in the dashboard once', async ({ page, playwright }) => {
    test.setTimeout(90_000);
    expect(await signIn(page, node, ADMIN_USER, adminPassword), 'admin sign-in').toBe(200);
    await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
    await page.goto(`${node.url}/access?tab=iam`);
    const create = main(page).getByRole('button', { name: 'New service account', exact: true });
    await expect(create).toBeEnabled({ timeout: 15_000 });
    await create.click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel(/^Namespace/).fill('apps');
    await dialog.getByLabel(/^Name(?!space)/).fill('e2e-robot');
    await dialog.getByLabel(/^Description/).fill('end-to-end');
    expect(await submitOnce(page, 'Create service account', 'POST', '/api/v1/iam/serviceaccounts'), 'single create').toBe(201);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    const sas = main(page).getByRole('table', { name: 'Service accounts' });
    await expect(sas.getByText('apps/e2e-robot')).toBeVisible({ timeout: 15_000 });

    await rowAction(page, 'Service accounts', 'New token for apps/e2e-robot');
    const tokenDialog = page.getByRole('dialog');
    await expect(tokenDialog.getByRole('button', { name: 'Issue token', exact: true })).toBeVisible({ timeout: 15_000 });
    await tokenDialog.getByLabel(/^Description/).fill('e2e once');
    expect(await submitOnce(page, 'Issue token', 'POST', '/api/v1/iam/serviceaccounts/apps/e2e-robot/tokens'), 'single issuance').toBe(201);
    const shown = page.getByRole('dialog').getByTestId('new-sa-token');
    await expect(shown).toBeVisible();
    const secret = ((await shown.textContent()) ?? '').match(/kdr_[A-Za-z0-9_-]+/)?.[0] ?? '';
    expect(secret.length, 'a kdr_ token is shown').toBeGreaterThan(8);

    // Use it once, outside the browser session: the API authenticates the account.
    const api = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    let principal: { kind: string; name: string };
    try {
      const used = await api.get(`${node.url}/api/v1/auth/me`, { headers: { Authorization: `Bearer ${secret}` } });
      expect(used.status(), 'the issued token authenticates').toBe(200);
      principal = ((await used.json()) as { principal: { kind: string; name: string } }).principal;
    } finally {
      await api.dispose();
    }
    expect(principal.kind).toBe('ServiceAccount');
    expect(principal.name).toBe('serviceaccount:apps:e2e-robot');

    // Closing the dialog drops the secret: it is not in the page any more.
    await page.getByRole('dialog').getByRole('button', { name: 'Done', exact: true }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    expect(await page.content()).not.toContain(secret);
    await expect(sas.getByRole('row').filter({ hasText: 'apps/e2e-robot' })).toContainText('1', { timeout: 15_000 });
  });

  test('edits and deletes an API-owned role binding and keeps its metadata', async ({ page }) => {
    test.setTimeout(90_000);
    expect(await signIn(page, node, ADMIN_USER, adminPassword), 'admin sign-in').toBe(200);
    await expect(main(page).getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible();
    const me = await page.request.get(`${node.url}/api/v1/auth/me`);
    const headers = { 'X-Kudurru-CSRF': ((await me.json()) as { csrf: string }).csrf };
    await me.dispose();
    const binding = (name: string, scope: { kind: string; name?: string }, subject: string) => ({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'RoleBinding',
      metadata: { name, namespace: 'default', labels: { team: 'dns' }, annotations: { 'example.com/ticket': 'OPS-12' } },
      spec: { role: 'viewer', scope, subjects: [{ kind: 'User', name: subject }] },
    });
    for (const b of [binding('e2e-rb', { kind: 'zonegroup', name: 'prod' }, 'e2e-viewer'), binding('e2e-rb-clients', { kind: 'clientgroup', name: 'kids' }, 'e2e-parent')]) {
      const r = await page.request.post(`${node.url}/api/v1/iam/rolebindings`, { data: b, headers });
      expect(r.status(), `create ${b.metadata.name}`).toBe(201);
      await r.dispose();
    }
    await page.goto(`${node.url}/access?tab=iam`);
    const table = main(page).getByRole('table', { name: 'Role bindings' });
    // The reserved clientgroup scope is not edited here (the form would send it back cluster-wide).
    await expect(table.getByRole('button', { name: 'Delete default/e2e-rb-clients', exact: true })).toBeEnabled({ timeout: 15_000 });
    await expect(table.getByRole('button', { name: 'Edit default/e2e-rb-clients', exact: true })).toBeDisabled();

    await rowAction(page, 'Role bindings', 'Edit default/e2e-rb');
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByRole('button', { name: 'Save changes', exact: true })).toBeDisabled({ timeout: 15_000 });
    await dialog.getByLabel(/^User name/).fill('e2e-viewer-2');
    expect(await submitOnce(page, 'Save changes', 'PUT', '/api/v1/iam/rolebindings/default/e2e-rb'), 'single replace').toBe(200);
    await expect(page.getByRole('dialog')).toHaveCount(0);
    const read = await page.request.get(`${node.url}/api/v1/iam/rolebindings/default/e2e-rb`);
    expect(read.status()).toBe(200);
    const doc = (await read.json()) as { metadata: { labels?: Record<string, string>; annotations?: Record<string, string> }; spec: { scope: unknown; subjects: unknown } };
    await read.dispose();
    expect(doc.spec.scope).toEqual({ kind: 'zonegroup', name: 'prod' });
    expect(doc.spec.subjects).toEqual([{ kind: 'User', name: 'e2e-viewer-2' }]);
    expect(doc.metadata.labels).toEqual({ team: 'dns' });
    expect(doc.metadata.annotations?.['example.com/ticket']).toBe('OPS-12');
    expect(doc.metadata.annotations?.['kudurru.io/managed-by']).toBe('api');

    for (const name of ['e2e-rb', 'e2e-rb-clients']) {
      await rowAction(page, 'Role bindings', `Delete default/${name}`);
      await expect(page.getByRole('dialog').getByRole('button', { name: 'Delete role binding', exact: true })).toBeVisible({ timeout: 15_000 });
      expect(await submitOnce(page, 'Delete role binding', 'DELETE', `/api/v1/iam/rolebindings/default/${name}`), `single delete ${name}`).toBe(204);
      await expect(page.getByRole('dialog')).toHaveCount(0);
      await expect(table.getByRole('button', { name: `Delete default/${name}`, exact: true })).toHaveCount(0, { timeout: 15_000 });
    }
  });
});
