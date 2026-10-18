import { expect, main, settle, test } from './fixtures';

test('the hover flyout collapses after mouse navigation without moving the page', async ({ page }) => {
  await page.goto('/tools');
  await settle(page);
  const nav = page.getByRole('navigation', { name: 'Side navigation' });
  await nav.getByRole('button', { name: 'Collapse sidebar' }).click();
  await main(page).hover();
  const railContent = await main(page).boundingBox();
  await expect(nav.getByRole('link', { name: 'Dashboard' })).toHaveText('');

  await nav.getByRole('link', { name: 'Dashboard' }).hover();
  await expect(nav.getByRole('link', { name: 'Query log' })).toHaveText('Query log');
  expect(await main(page).boundingBox()).toEqual(railContent);
  await nav.getByRole('link', { name: 'Query log' }).click();
  await expect(page).toHaveURL((url) => url.pathname === '/queries');
  await settle(page);
  await main(page).hover();
  // Moving the pointer alone must close the flyout; no extra click to blur it.
  await expect(nav.getByRole('link', { name: 'Query log' })).toHaveText('');
  await expect(nav.getByRole('link', { name: 'Query log' })).toHaveAttribute('aria-current', 'page');
  expect(await main(page).boundingBox()).toEqual(railContent);
});

test('keyboard focus keeps a revealed flyout open until focus leaves', async ({ page }) => {
  await page.goto('/tools');
  await settle(page);
  const nav = page.getByRole('navigation', { name: 'Side navigation' });
  await nav.getByRole('button', { name: 'Collapse sidebar' }).click();
  await main(page).hover();
  await nav.getByRole('link', { name: 'Dashboard' }).hover();
  await nav.getByRole('link', { name: 'Dashboard' }).focus();
  await page.keyboard.press('Tab');
  await expect(nav.getByRole('link', { name: 'Query log' })).toBeFocused();
  await main(page).hover();
  await expect(nav.getByRole('link', { name: 'Query log' })).toHaveText('Query log');

  await main(page).click();
  await expect(nav.getByRole('link', { name: 'Query log' })).toHaveText('');
});
