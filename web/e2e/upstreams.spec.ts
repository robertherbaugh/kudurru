// The Upstreams page: the groups of upstreams.groups (GET /settings) with
// the health of each server (GET /stats/upstreams servers[]).
import { env, expect, expectRendered, main, settle, test } from './fixtures';

test('/upstreams lists the configured group and its server', async ({ page }) => {
  await page.goto('/upstreams');
  await settle(page);
  await expectRendered(page);
  await expect(main(page).getByRole('heading', { level: 1 })).toHaveText(/upstreams/i);
  await expect(main(page)).toContainText('default');
  // The table links each group to its page, which lists the servers.
  await main(page).getByRole('link', { name: 'default', exact: true }).click();
  await expect(page).toHaveURL(/\/upstreams\/default$/);
  await settle(page);
  await expect(main(page)).toContainText(env('KUDURRU_E2E_UPSTREAM'));
  await expectRendered(page);
});

test('the sidebar links the Upstreams page', async ({ page }) => {
  await page.goto('/');
  await settle(page);
  const link = page.getByRole('navigation', { name: 'Side navigation' }).getByRole('link', { name: 'Upstreams', exact: true });
  await expect(link).toHaveAttribute('href', '/upstreams');
});
