// Deep links: a reload of a client route whose last segment has a dot
// (a zone name) gets index.html, not the static handler's 404; missing
// asset files still 404.
import { expect, expectRendered, main, settle, test } from './fixtures';
import { ZONE } from './support/kudurru';

test('reload of /zones/<object with dots> renders the zone', async ({ page }) => {
  const res = await page.goto(`/zones/${ZONE.object}`);
  expect(res?.status(), `GET /zones/${ZONE.object}`).toBe(200);
  await settle(page);
  await expectRendered(page);
  await expect(main(page)).toContainText(ZONE.host);
});

test('reload of /zones/<apex with trailing dot> renders the SPA', async ({ page }) => {
  const res = await page.goto(`/zones/${ZONE.apex}`);
  expect(res?.status(), `GET /zones/${ZONE.apex}`).toBe(200);
  expect(res?.headers()['content-type']).toContain('text/html');
  await expect(page.getByRole('navigation', { name: 'Side navigation' })).toBeVisible();
  await expect(main(page).getByRole('heading', { level: 1 }).first()).toBeVisible();
});

test('navigating from the zone list to the zone and reloading', async ({ page }) => {
  await page.goto('/zones');
  await settle(page);
  await main(page).getByRole('link', { name: ZONE.object, exact: true }).click();
  await expect(page).toHaveURL((u) => decodeURIComponent(u.pathname) === `/zones/${ZONE.object}`);
  await settle(page);
  await page.reload();
  await settle(page);
  await expectRendered(page);
  await expect(main(page)).toContainText(ZONE.host);
});

test('a missing asset is a 404, not index.html', async ({ request }) => {
  const res = await request.get('/assets/does-not-exist-e2e.js');
  expect(res.status()).toBe(404);
});
