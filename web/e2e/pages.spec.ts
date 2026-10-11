// Every page of the navigation (and the detail and tab views behind it)
// renders against the real API in both colour schemes: no ErrorBoundary,
// no failed query, no console error or uncaught exception, and no
// undefined/NaN/[object Object] in the content.
import { expect, expectRendered, main, settle, test } from './fixtures';
import { ZONE } from './support/kudurru';

/** Every navigation entry (path, sidebar label). */
const NAV = [
  ['/', 'Dashboard'],
  ['/queries', 'Query log'],
  ['/history', 'Long-term'],
  ['/lists', 'Lists'],
  ['/groups', 'Groups'],
  ['/clients', 'Clients'],
  ['/policies', 'Policies'],
  ['/zones', 'Zones'],
  ['/upstreams', 'Upstreams'],
  ['/nodes', 'Nodes'],
  ['/settings', 'Settings'],
  ['/tools', 'Tools'],
  ['/messages', 'Diagnosis'],
  ['/access', 'Access & audit'],
] as const;

/** Views that are not navigation entries of their own. */
const MORE = [`/zones/${ZONE.object}`, '/settings?view=expert', '/access?tab=iam', '/access?tab=audit'];

test('the sidebar links exactly the pages this suite covers', async ({ page }) => {
  await page.goto('/');
  const nav = page.getByRole('navigation', { name: 'Side navigation' });
  await expect(nav.getByRole('link').first()).toBeVisible();
  const hrefs = await nav.getByRole('link').evaluateAll((links) => links.map((a) => a.getAttribute('href')));
  expect(hrefs.sort()).toEqual(NAV.map(([p]) => p).sort());
});

for (const colorScheme of ['light', 'dark'] as const) {
  test.describe(`${colorScheme} scheme`, () => {
    test.use({ colorScheme });

    for (const [path, label] of NAV) {
      test(`${label} (${path}) renders`, async ({ page }) => {
        await page.goto(path);
        await settle(page);
        await expectRendered(page);
        // The sidebar marks the page as current.
        await expect(page.getByRole('navigation', { name: 'Side navigation' }).getByRole('link', { name: label, exact: true })).toHaveAttribute('aria-current', 'page');
      });
    }

    for (const path of MORE) {
      test(`${path} renders`, async ({ page }) => {
        await page.goto(path);
        await settle(page);
        await expectRendered(page);
        await expect(main(page)).not.toBeEmpty();
      });
    }

    test('client-side navigation through every page keeps the shell alive', async ({ page }) => {
      await page.goto('/');
      await settle(page);
      const nav = page.getByRole('navigation', { name: 'Side navigation' });
      for (const [path, label] of NAV) {
        await nav.getByRole('link', { name: label, exact: true }).click();
        await expect(page).toHaveURL((u) => u.pathname === path);
        await settle(page);
        await expectRendered(page);
      }
    });
  });
}
