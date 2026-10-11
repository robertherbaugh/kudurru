// Settings shows the values the server runs with (GET /settings answers
// the envelope {scope,node,settings,...}; the values are under .settings).
import { env, expect, expectRendered, settle, test } from './fixtures';

test.beforeEach(async ({ page }) => {
  await page.goto('/settings');
  await settle(page);
});

test('blocking.enabled shows its real value (on)', async ({ page }) => {
  const row = page.locator('[data-configkey="blocking.enabled"]');
  await expect(row.getByRole('switch')).toBeChecked();
});

test('querylog.enabled shows its real value (on)', async ({ page }) => {
  const row = page.locator('[data-configkey="querylog.enabled"]');
  await expect(row.getByRole('switch')).toBeChecked();
});

test('upstreams.groups holds the configured group and server', async ({ page }) => {
  const row = page.locator('[data-configkey="upstreams.groups"]');
  const value = row.locator('textarea, pre').first();
  await expect(value).toBeVisible();
  const text = () => value.evaluate((el) => (el instanceof HTMLTextAreaElement ? el.value : (el.textContent ?? '')));
  await expect.poll(text, { message: 'upstreams.groups shows the configured server' }).toContain(env('KUDURRU_E2E_UPSTREAM'));
  expect(await text()).toContain('default');
  await expectRendered(page);
});
