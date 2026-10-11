// Dialogs and wizards on a short viewport: a visible close button, a body
// that scrolls so the footer's buttons can be reached, and closing or
// cancelling through them. "New list" is a wizard page (/lists/new) since
// the UX round 2; "Add domains" stays a dialog.
import type { Locator, Page } from '@playwright/test';
import { expect, settle, test } from './fixtures';

test.use({ viewport: { width: 1280, height: 560 } });

/** Scrolls with the wheel over box, as a user would, until target is fully in view. */
async function wheelUntilVisible(page: Page, box: Locator, target: Locator): Promise<void> {
  const b = await box.boundingBox();
  expect(b, 'scroll box').not.toBeNull();
  await page.mouse.move(b!.x + b!.width / 2, b!.y + Math.min(b!.height / 2, 200));
  for (let i = 0; i < 15; i++) {
    if (await target.evaluate((el) => { const r = el.getBoundingClientRect(); return r.bottom <= window.innerHeight && r.top >= 0; })) break;
    await page.mouse.wheel(0, 400);
    await page.waitForTimeout(100);
  }
}

test('Lists → New list: the wizard page keeps Next and Cancel reachable, Cancel goes back', async ({ page }) => {
  await page.goto('/lists');
  await settle(page);
  await page.getByRole('link', { name: 'New list' }).click();
  await expect(page).toHaveURL(/\/lists\/new$/);
  await expect(page.getByRole('heading', { level: 1, name: 'New list' })).toBeVisible();

  // The Sources step is the longest: the sticky footer stays reachable.
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: 'Well-known lists' })).toBeVisible();
  const next = page.getByRole('button', { name: 'Next' });
  const cancel = page.getByRole('button', { name: 'Cancel' });
  await wheelUntilVisible(page, page.locator('.k-wizard'), next);
  await expect(next, 'the Next button scrolls into view').toBeInViewport({ ratio: 1 });
  await expect(cancel, 'the Cancel button scrolls into view').toBeInViewport({ ratio: 1 });

  // Next without a source stays on the step and says why.
  await next.click();
  await expect(page.getByText(/Add at least one source/)).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('radio', { name: /Block list/ })).toBeVisible();

  await page.getByRole('button', { name: 'Cancel' }).click();
  await expect(page).toHaveURL(/\/lists$/);
});

test('Lists → Add domains: close button, reachable footer, closes', async ({ page }) => {
  await page.goto('/lists');
  await settle(page);
  await page.getByRole('button', { name: 'Add domains' }).click();
  const dialog = page.getByRole('dialog', { name: 'Add domains' });
  await expect(dialog).toBeVisible();

  const close = dialog.getByRole('button', { name: /^close/i }).first();
  await expect(close, 'the dialog has a visible close button').toBeVisible();
  await expect(close).toBeInViewport();

  const add = dialog.getByRole('button', { name: /^Add/ });
  await wheelUntilVisible(page, dialog, add);
  await expect(add, 'the Add button is in view').toBeInViewport({ ratio: 1 });
  const dialogBox = (await dialog.boundingBox())!;
  const addBox = (await add.boundingBox())!;
  expect(addBox.y + addBox.height, 'Add button inside the dialog').toBeLessThanOrEqual(dialogBox.y + dialogBox.height + 1);

  await close.scrollIntoViewIfNeeded();
  await close.click();
  await expect(dialog).toBeHidden();
});
