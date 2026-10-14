// Pages that show what the global setup's traffic and objects produced:
// the dashboard tiles, a blocked query-log row, the clients, the node, the
// audit log and an API token created through the UI.
import { badLines, env, expect, expectRendered, main, settle, test } from './fixtures';
import { BLOCKLIST } from './support/kudurru';

/** The number a stat tile shows ("2.9k" and "1,234" included). */
function tileNumber(text: string): number {
  const m = /([\d.,]+)\s*([kKmM]?)/.exec(text.replace(/\s+/g, ' '));
  if (!m?.[1]) return NaN;
  const n = Number(m[1].replace(/,/g, ''));
  return m[2]?.toLowerCase() === 'k' ? n * 1e3 : m[2]?.toLowerCase() === 'm' ? n * 1e6 : n;
}

test('dashboard: the tiles count the traffic and the domains on lists', async ({ page }) => {
  await page.goto('/');
  await settle(page);
  await expect.poll(async () => tileNumber(await page.getByTestId('tile-total').innerText()), { message: 'Total queries' }).toBeGreaterThan(0);
  await expect.poll(async () => tileNumber(await page.getByTestId('tile-blocked').innerText()), { message: 'Queries blocked' }).toBeGreaterThan(0);
  // GET /stats/summary domains_on_lists comes from the compiled lists.
  await expect
    .poll(
      async () => {
        await page.reload();
        await settle(page);
        return tileNumber(await page.getByTestId('tile-lists').innerText());
      },
      { message: 'Domains on lists', timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  await expectRendered(page);
});

test('query log: a blocked row explains itself and links to the Allowlist add dialog', async ({ page }) => {
  // The global setup's traffic: ads.example.net is blocked by the run's file list.
  await page.goto(`/queries?status_class=blocked&domain=${BLOCKLIST.blocked}`);
  await settle(page);
  const row = main(page).getByRole('table', { name: 'Query log' }).locator('tr[aria-expanded]').filter({ hasText: BLOCKLIST.blocked }).first();
  await expect(row).toBeVisible();
  await row.focus();
  await page.keyboard.press('Enter');
  await expect(row).toHaveAttribute('aria-expanded', 'true');
  const why = main(page).getByRole('group', { name: /^Why ads\.example\.net\.? got this answer$/ });
  await expect(why).toContainText('Matched list');
  await expect(why).toContainText('Policy');
  // No unblock request names it, so the link opens the Allowlist's Add dialog with the name.
  await expect(why.getByRole('link', { name: 'Allow…' })).toHaveAttribute('href', `/allowlist?add=${BLOCKLIST.blocked}`);
  await expectRendered(page);
});

test('clients: 127.0.0.1 is listed with its queries', async ({ page }) => {
  await page.goto('/clients');
  await settle(page);
  const table = main(page).getByRole('table', { name: /clients/i });
  const row = table.getByRole('row').filter({ hasText: '127.0.0.1' });
  await expect(row).toHaveCount(1);
  expect(badLines(await row.innerText())).toEqual([]);
  await expectRendered(page);
});

test('nodes: the node is listed, ready, with its health', async ({ page }) => {
  const node = env('KUDURRU_E2E_NODE');
  await page.goto('/nodes');
  await settle(page);
  const row = main(page).getByRole('table', { name: /nodes/i }).getByRole('row').filter({ hasText: node });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText('ready');
  await expect(row).not.toContainText('not ready');
  await row.getByRole('button', { name: 'Health' }).click();
  const health = main(page).getByRole('region', { name: `Health of ${node}` });
  await expect(health).toBeVisible();
  await settle(page);
  await expectRendered(page);
});

test('audit: the audit page lists entries', async ({ page }) => {
  await page.goto('/audit');
  await settle(page);
  const rows = main(page).getByRole('table', { name: /audit/i }).locator('tbody tr');
  await expect.poll(() => rows.count(), { message: 'audit rows' }).toBeGreaterThan(0);
  await expectRendered(page);
});

test('access: creating an API token shows it with its description', async ({ page }) => {
  const description = `e2e token ${Date.now()}`;
  await page.goto('/access');
  await settle(page);
  const panel = main(page).getByRole('region', { name: 'API tokens' });
  // The first field names the token (description on the wire).
  await panel.getByRole('textbox').first().fill(description);
  await panel.getByRole('button', { name: 'Create token' }).click();
  await expect(panel.getByTestId('new-token')).toContainText(/kdr_/);
  await expect(panel).toContainText(description);
  const row = panel.getByRole('table', { name: /tokens/i }).getByRole('row').filter({ hasText: description });
  await expect(row).toHaveCount(1);
  expect(badLines(await panel.innerText())).toEqual([]);
});
