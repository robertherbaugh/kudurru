// Dialogs on a short viewport: a visible close button, a body that scrolls
// so the footer's buttons can be reached, and closing through the button.
import { expect, settle, test } from './fixtures';

test.use({ viewport: { width: 1280, height: 560 } });

test('Lists → New list: close button, scrollable body, reachable footer', async ({ page }) => {
  await page.goto('/lists');
  await settle(page);
  await page.getByRole('button', { name: 'New list' }).click();
  const dialog = page.getByRole('dialog', { name: 'New list' });
  await expect(dialog).toBeVisible();

  const close = dialog.getByRole('button', { name: /^close/i });
  await expect(close, 'the dialog has a visible close button').toBeVisible();
  await expect(close).toBeInViewport();

  // Scroll the dialog body with the wheel, as a user would.
  const create = dialog.getByRole('button', { name: 'Create list' });
  const cancel = dialog.getByRole('button', { name: 'Cancel' });
  const box = await dialog.boundingBox();
  expect(box, 'dialog box').not.toBeNull();
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
  for (let i = 0; i < 10; i++) {
    if (await create.evaluate((el) => { const r = el.getBoundingClientRect(); return r.bottom <= window.innerHeight && r.top >= 0; })) break;
    await page.mouse.wheel(0, 400);
    await page.waitForTimeout(100);
  }
  await expect(create, 'the Create button scrolls into view').toBeInViewport({ ratio: 1 });
  await expect(cancel, 'the Cancel button scrolls into view').toBeInViewport({ ratio: 1 });
  // The footer is inside the dialog's box, not clipped below it.
  const dialogBox = (await dialog.boundingBox())!;
  const createBox = (await create.boundingBox())!;
  expect(createBox.y + createBox.height, 'Create button inside the dialog').toBeLessThanOrEqual(dialogBox.y + dialogBox.height + 1);

  // The close button is still reachable after scrolling.
  await close.scrollIntoViewIfNeeded();
  await expect(close).toBeInViewport();
  await close.click();
  await expect(dialog).toBeHidden();
});
