// Settings shows the values the server runs with (GET /settings answers
// the envelope {scope,node,settings,...}; the values are under .settings),
// each key with the typed control of its C1 metadata.
import { env, expect, expectRendered, main, settle, test } from './fixtures';

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

test('upstreams.groups links the configured group, whose page shows its server', async ({ page }) => {
  const row = page.locator('[data-configkey="upstreams.groups"]');
  await expect(row.getByRole('link', { name: 'Edit on the Upstreams page' })).toHaveAttribute('href', '/upstreams');
  await row.getByRole('link', { name: 'default', exact: true }).click();
  await expect(page).toHaveURL(/\/upstreams\/default$/);
  await settle(page);
  await expect(main(page).getByRole('table', { name: 'Servers of default' })).toContainText(env('KUDURRU_E2E_UPSTREAM'));
  await expectRendered(page);
});

test('an enum key is a select that saves and shows the new value', async ({ page }) => {
  // blocking.initStrategy only takes effect at the next start: harmless here.
  await page.goto('/settings?view=expert&q=initStrategy');
  await settle(page);
  const row = page.locator('[data-configkey="blocking.initStrategy"]');
  const select = row.getByRole('combobox', { name: 'List loading at start' });
  const before = await select.inputValue();
  // Something other than the default (fast), so the row shows "changed".
  const next = before === 'failOnError' ? 'blocking' : 'failOnError';
  await select.selectOption(next);
  await row.getByRole('button', { name: 'Save' }).click();
  await expect(row.getByText('changed', { exact: true })).toBeVisible();
  await page.reload();
  await settle(page);
  await expect(row.getByRole('combobox', { name: 'List loading at start' })).toHaveValue(next);
  // Back to what the suite started with.
  await row.getByRole('combobox', { name: 'List loading at start' }).selectOption(before);
  await row.getByRole('button', { name: 'Save' }).click();
  await expect(row.getByRole('combobox', { name: 'List loading at start' })).toHaveValue(before);
  await expect(row.getByRole('button', { name: 'Save' })).toHaveCount(0);
});

test('number keys are bounded inputs and durations are preset selects', async ({ page }) => {
  await page.goto('/settings?view=expert&q=ratelimit.slip');
  await settle(page);
  const slip = page.locator('[data-configkey="ratelimit.slip"]').getByRole('spinbutton');
  await expect(slip).toHaveAttribute('max', '100');
  await slip.fill('150');
  await expect(page.locator('[data-configkey="ratelimit.slip"]')).toContainText('Between 0 and 100');
  await page.goto('/settings?view=expert&q=blockTTL');
  await settle(page);
  await expect(page.locator('[data-configkey="blocking.blockTTL"]').getByRole('combobox', { name: 'Block response TTL' })).toHaveValue('10m');
});

test('the block page section previews the page', async ({ page }) => {
  const section = main(page).getByRole('region', { name: 'Block page' });
  await expect(section.getByTestId('blockpage-intro')).toContainText('certificate warning');
  // The frame is sandboxed without scripts, so Playwright cannot look
  // inside it: check that the framed request succeeds (cookie auth) and
  // what it answers.
  const framed = page.waitForResponse((r) => r.url().includes('/api/v1/blockpage/preview?domain=ads.example.com'));
  await section.getByRole('button', { name: 'Preview' }).click();
  const dialog = page.getByRole('dialog', { name: 'Block page preview' });
  await expect(dialog.locator('iframe[title="Block page for ads.example.com"]')).toHaveAttribute('sandbox', '');
  const res = await framed;
  expect(res.status()).toBe(200);
  expect(await res.text()).toContain('ads.example.com');
});
