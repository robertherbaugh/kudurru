// Sign-in: the local login form, a rejected password, sign-out, and the
// first-boot flow of a generated admin password (hasDefaultCredentials,
// mustChangePassword) on a server of its own.
import { env, expect, main, settle, SIGNED_OUT_401, test } from './fixtures';
import { ADMIN_USER, startServer, type Server } from './support/kudurru';

// Before the password is changed the session may do nothing else (403s).
const SIGNED_OUT_OR_403 = /status of (401|403)/;

test.describe('signed out', () => {
  test.use({ storageState: { cookies: [], origins: [] }, allowConsole: SIGNED_OUT_401 });

  test('signs in with the admin password, survives a reload and signs out', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(env('KUDURRU_E2E_ADMIN_PASSWORD'));
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();
    // The password came from a file: no default-credentials nag.
    await expect(page.getByText(/default (password|credentials)/i)).toHaveCount(0);

    await page.reload();
    await settle(page);
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();

    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  });

  test('rejects a wrong password', async ({ page }) => {
    await page.goto('/');
    // A user that does not exist: no failed-login lockout for the shared admin.
    await page.getByRole('textbox', { name: 'Username' }).fill('nobody');
    await page.getByLabel(/^Password/).fill('not-the-password-at-all');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByRole('alert')).toContainText('current credential is invalid or expired');
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  });
});

test.describe('first boot with a generated password', () => {
  let server: Server;
  test.use({ storageState: { cookies: [], origins: [] }, allowConsole: SIGNED_OUT_OR_403 });

  test.beforeAll(async () => {
    server = await startServer(env('KUDURRU_E2E_BIN_PATH'), { upstream: env('KUDURRU_E2E_UPSTREAM'), node: 'e2e-firstboot' });
  });
  test.afterAll(async () => {
    await server?.stop();
  });

  test('warns about the default credentials and makes the admin change the password', async ({ page }) => {
    const newPassword = `changed-${Date.now()}-password`;
    await page.goto(server.url);
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await expect(page.getByText('Default credentials are active')).toBeVisible();
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(server.adminPassword);
    await page.getByRole('button', { name: 'Sign in' }).click();

    // mustChangePassword: the session may only change the password.
    // Text boxes only: the dialog itself is named "Choose a new password".
    const newField = page.getByRole('textbox', { name: /new password/i }).first();
    await expect(newField, 'a change-password form after signing in with the generated password').toBeVisible();
    const current = page.getByRole('textbox', { name: /current password/i });
    if (await current.count()) await current.fill(server.adminPassword);
    await newField.fill(newPassword);
    const confirm = page.getByRole('textbox', { name: /confirm|repeat/i });
    if (await confirm.count()) await confirm.fill(newPassword);
    await page.locator('dialog[open]').getByRole('button', { name: /change password|set password|save|update/i }).click();

    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();
    await settle(page);
    await expect(page.getByText(/default (password|credentials)/i)).toHaveCount(0);
    const status = await page.request.get(`${server.url}/api/v1/auth/status`);
    expect(((await status.json()) as { hasDefaultCredentials?: boolean }).hasDefaultCredentials).toBe(false);
  });
});
