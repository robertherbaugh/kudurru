// Tools → Query (dig): POST /dns/query answers presentation lines
// (string[]), which the page must show as they are.
import type { Page } from '@playwright/test';
import { badLines, expect, settle, test } from './fixtures';
import { BLOCKLIST, ZONE } from './support/kudurru';

async function dig(page: Page, name: string) {
  await page.goto('/tools');
  await settle(page);
  const panel = page.getByRole('region', { name: 'Query (dig)' });
  await panel.getByRole('textbox', { name: 'Name' }).fill(name);
  await panel.getByRole('button', { name: 'Query' }).click();
  const result = panel.locator('[aria-live="polite"]').last();
  await expect(result).toBeVisible();
  return result;
}

test('dig of a name in the local zone shows its record', async ({ page }) => {
  const result = await dig(page, ZONE.host);
  await expect(result).toContainText(ZONE.hostA);
  await expect(result).toContainText(ZONE.host);
  await expect(result).toContainText('NOERROR');
  expect(badLines(await result.innerText()), 'dig output lines with undefined/NaN').toEqual([]);
});

test('dig of a blocked domain shows the blocked status and the list', async ({ page }) => {
  const result = await dig(page, BLOCKLIST.blocked);
  await expect(result).toContainText(/blocked/i);
  await expect(result).toContainText(BLOCKLIST.name);
  expect(badLines(await result.innerText()), 'dig output lines with undefined/NaN').toEqual([]);
});
