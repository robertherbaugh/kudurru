// Chapter: watch the network (Dashboard, Query log, Long-term).
import { choose, toTop, fresh, getPage, go, group, scroll, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('monitor', async () => {
    await fresh();
    const page = getPage();
    await shot('dashboard');
    await scroll(800);
    await shot('dashboard-lower');
    await toTop();
    await page.getByRole('combobox', { name: 'Node scope' }).click();
    await settle(500);
    await shot('node-selector');
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: 'Disable blocking' }).click();
    await settle(600);
    await shot('disable-blocking');
    await page.keyboard.press('Escape');
    await go('/queries');
    await shot('query-log');
    await choose(page.getByRole('combobox', { name: 'Status' }), 'Blocked');
    await settle(1200);
    await page.locator('tr.k-log-row').first().click();
    await settle(800);
    await toTop();
    await shot('query-detail');
    await toTop();
    await page.getByRole('button', { name: 'Purge…' }).click();
    await settle(700);
    await shot('purge-query-log');
    await page.keyboard.press('Escape');
    await go('/history');
    await shot('long-term');
  });
}
