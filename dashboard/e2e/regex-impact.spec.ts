import path from 'node:path';
import { expect, settle, test } from './fixtures';
import { adminApi } from './support/api';
import { BLOCKLIST } from './support/kudurru';

test('a regex inspection explains the real opposing rule without changing data', async ({ page }) => {
  await page.goto('/');
  const api = await adminApi(page);
  const expression = '^ads\\.example\\.net$';
  const created = await api.post<{ items: { id: string }[] }>('/allowlist', { type: 'regex', values: [expression] });
  const id = created.items[0]!.id;
  try {
    await page.goto('/allowlist');
    await settle(page);
    await page.getByTestId(`entry-${expression}`).getByRole('button', { name: 'Test a domain' }).click();
    const dialog = page.getByRole('dialog', { name: 'Test a domain against this regex' });
    await dialog.getByRole('textbox', { name: 'Domain', exact: true }).fill(BLOCKLIST.blocked);
    await dialog.getByRole('button', { name: 'Check domain' }).click();
    await expect(dialog).toContainText(`${BLOCKLIST.blocked}. matches this expression.`);
    await expect(dialog).toContainText('Matching block lists this allow entry may override');
    await expect(dialog.getByRole('link', { name: new RegExp(BLOCKLIST.name) })).toHaveAttribute('href', `/lists/default/${BLOCKLIST.name}`);
    await expect(dialog).toContainText('Policies, schedules, modifiers and other rules');
    await page.screenshot({ path: path.resolve('..', '.context', 'regex-impact.png') });
    await dialog.getByRole('textbox', { name: 'Domain', exact: true }).fill('different.example');
    await dialog.getByRole('button', { name: 'Check domain' }).click();
    await expect(dialog).toContainText('different.example. does not match this expression.');
    await expect(dialog.getByRole('link', { name: new RegExp(BLOCKLIST.name) })).toHaveCount(0);
  } finally {
    await api.del(`/allowlist/${encodeURIComponent(id)}`);
  }
});
