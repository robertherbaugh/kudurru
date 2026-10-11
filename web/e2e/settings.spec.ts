// Settings shows the values the server runs with (GET /settings answers
// the envelope {scope,node,settings,...}; the values are under .settings),
// each key with the typed control of its C1 metadata.
import { env, expect, expectRendered, main, selector, settle, test } from './fixtures';
import { choose } from './support/astryx';

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

test('an enum key is a Selector that saves and shows the new value', async ({ page }) => {
  // blocking.initStrategy only takes effect at the next start: harmless here.
  await page.goto('/settings?view=expert&q=initStrategy');
  await settle(page);
  const row = page.locator('[data-configkey="blocking.initStrategy"]');
  const select = selector(row, 'List loading at start');
  const before = (await select.getAttribute('data-value')) ?? 'fast';
  // Something other than the default (fast), so the row shows "changed".
  const next = before === 'failOnError' ? 'blocking' : 'failOnError';
  const labels: Record<string, string> = { fast: 'Fast', blocking: 'Wait for lists', failOnError: 'Fail on error' };
  await choose(select, labels[next] ?? next);
  // The chosen option's help (server metadata) shows under the control and describes it.
  await expect(select).toHaveAccessibleDescription(/load/i);
  await row.getByRole('button', { name: 'Save' }).click();
  await expect(row.getByText('changed', { exact: true })).toBeVisible();
  await page.reload();
  await settle(page);
  await expect(selector(row, 'List loading at start')).toHaveAttribute('data-value', next);
  // Back to what the suite started with.
  await choose(selector(row, 'List loading at start'), labels[before] ?? before);
  await row.getByRole('button', { name: 'Save' }).click();
  await expect(selector(row, 'List loading at start')).toHaveAttribute('data-value', before);
  await expect(row.getByRole('button', { name: 'Save' })).toHaveCount(0);
});

test('number keys are bounded inputs and durations are preset Selectors', async ({ page }) => {
  await page.goto('/settings?view=expert&q=ratelimit.slip');
  await settle(page);
  const row = page.locator('[data-configkey="ratelimit.slip"]');
  const slip = row.getByRole('spinbutton');
  await expect(row).toContainText('Range 0–100.');
  await slip.fill('150');
  await expect(row).toContainText('Between 0 and 100');
  await page.goto('/settings?view=expert&q=blockTTL');
  await settle(page);
  await expect(selector(page.locator('[data-configkey="blocking.blockTTL"]'), 'Block response TTL')).toHaveAttribute('data-value', '10m');
});

test('a settings row puts the explanation left (60%) and the control right (40%), help under the control', async ({ page }) => {
  await page.setViewportSize({ width: 1400, height: 900 });
  await page.goto('/settings?view=expert&q=defaultResponse');
  await settle(page);
  const row = page.locator('[data-configkey="blocking.defaultResponse"]');
  const label = await row.getByText('Default block response', { exact: true }).first().boundingBox();
  const control = await selector(row, 'Default block response').boundingBox();
  const box = await row.boundingBox();
  expect(label && control && box).toBeTruthy();
  // The control column starts at about 60% of the row.
  expect((control!.x - box!.x) / box!.width).toBeGreaterThan(0.55);
  expect(control!.x).toBeGreaterThan(label!.x + label!.width);
  // The selected option's help is under the control, not between label and control.
  const ids = ((await selector(row, 'Default block response').getAttribute('aria-describedby')) ?? '').split(' ');
  const helpId = ids.find((i) => i.endsWith('-help'));
  expect(helpId).toBeTruthy();
  const help = await row.locator(`[id="${helpId}"]`).boundingBox();
  expect(help!.y).toBeGreaterThan(control!.y + control!.height - 1);
});

test.describe('block page', () => {
  // Playwright injects its utility script into every frame; Chrome logs
  // refusing it in the script-less sandbox. A user's browser logs nothing.
  test.use({ allowConsole: /Blocked script execution in .*\/api\/v1\/blockpage\/preview/ });

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
});
