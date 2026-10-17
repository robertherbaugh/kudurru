// Dialogs and wizards on a short viewport: a visible close button, a body
// that scrolls so the footer's buttons can be reached, and closing or
// cancelling through them. "New list" is a wizard in a dialog over the
// Lists page (?new=1) that asks before discarding what was entered; "Add
// domains" is a plain dialog.
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

test('Lists → New list: the wizard dialog keeps Next, Back and Cancel in view, Cancel closes it', async ({ page }) => {
  await page.goto('/lists');
  await settle(page);
  await page.getByRole('button', { name: 'New list' }).click();
  await expect(page).toHaveURL(/\/lists\?new=1$/);
  const dialog = page.getByRole('dialog', { name: 'New list' });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('button', { name: /^close/i }).first(), 'the dialog has a visible close button').toBeInViewport();

  // The Sources step is the longest: its body scrolls, the footer stays put.
  await dialog.getByRole('button', { name: 'Next' }).click();
  await expect(dialog.getByRole('heading', { level: 3, name: 'Sources' })).toBeFocused();
  await expect(dialog.getByRole('group', { name: 'Well-known lists' })).toBeVisible();
  const next = dialog.getByRole('button', { name: 'Next' });
  for (const name of ['Next', 'Back', 'Cancel']) {
    await expect(dialog.getByRole('button', { name }), `${name} is in view without scrolling`).toBeInViewport({ ratio: 1 });
  }
  const last = dialog.getByRole('checkbox').last();
  await wheelUntilVisible(page, dialog, last);
  await expect(last, 'the end of the step scrolls into view').toBeInViewport();
  await expect(next, 'Next is still in view after scrolling').toBeInViewport({ ratio: 1 });

  // Next without a source stays on the step and says why.
  await next.click();
  await expect(dialog.getByText(/Add at least one source/)).toBeVisible();
  await dialog.getByRole('button', { name: 'Back' }).click();
  await expect(dialog.getByRole('radio', { name: /Block list/ })).toBeVisible();

  // Nothing entered: Cancel closes without asking.
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toBeHidden();
  await expect(page.getByRole('alertdialog')).toHaveCount(0);
  await expect(page).toHaveURL(/\/lists$/);
});

test('Lists → New list: ✕ on a dirty wizard asks before discarding; the backdrop does not close it', async ({ page }) => {
  await page.goto('/lists?new=1');
  const dialog = page.getByRole('dialog', { name: 'New list' });
  await expect(dialog).toBeVisible();
  await dialog.getByRole('radio', { name: /Allow list/ }).check();

  // A click on the backdrop (outside the dialog box) keeps it open.
  await page.mouse.click(5, 5);
  await expect(dialog).toBeVisible();

  await dialog.getByRole('button', { name: /^close/i }).first().click();
  const confirm = page.getByRole('alertdialog', { name: 'Discard the new list?' });
  await expect(confirm).toBeVisible();
  await confirm.getByRole('button', { name: 'Keep editing' }).click();
  await expect(confirm).toBeHidden();
  await expect(dialog.getByRole('radio', { name: /Allow list/ })).toBeChecked();

  // Escape asks too; Discard closes the wizard and drops ?new=1.
  await page.keyboard.press('Escape');
  await expect(confirm).toBeVisible();
  await confirm.getByRole('button', { name: 'Discard' }).click();
  await expect(dialog).toBeHidden();
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
