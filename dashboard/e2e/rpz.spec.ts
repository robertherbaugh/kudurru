// Real browser workflow (D391): create a block list with an RPZ feed in the
// wizard, watch this node transfer it from a stub primary (e2e/support/rpz.ts),
// read its state, serial, rules and ignored owners on the list page, then
// stop the primary and see the copy expire with the SERVFAIL warning, on the
// list page and on the Lists page, while DNS answers SERVFAIL.
import fs from 'node:fs';
import path from 'node:path';
import { expect, env, main, settle, test } from './fixtures';
import { adminApi, object, suffix } from './support/api';
import { REPO_DIR } from './support/kudurru';
import { startRPZPrimary } from './support/rpz';
import type { RPZFeedList } from '../src/api/types';

// Right after the list is created the node's configuration may not have it
// yet, and the list page's feed read answers 404 until it does.
test.use({ allowConsole: /status of 404 .*\/rpz\b/ });

test('an RPZ feed is created in the wizard, transferred, shown and expires on this node', async ({ page }) => {
  test.setTimeout(120_000);
  const id = suffix();
  const name = `e2e-rpz-${id}`;
  const zone = `rpz-${id}.test.`;
  const blocked = `blocked-${id}.example`;
  const client = `198.51.100.${10 + Math.floor(Math.random() * 200)}`;
  const primary = await startRPZPrimary(zone, { serial: 7, expire: 8, blocked: [blocked] });
  const api = await adminApi(page);
  const screenshotDir = path.join(REPO_DIR, '.context');
  fs.mkdirSync(screenshotDir, { recursive: true });
  const feedState = async () => (await api.get<RPZFeedList>(`/blocklists/default/${name}/rpz`)).body?.items?.[0]?.state;
  const dig = (q: string) => api.post<{ rcode: string; status_class: string }>('/dns/query', { name: q, type: 'A', client });
  try {
    await api.put(`/objects/clientgroups/default/${name}`, object('ClientGroup', name, { clients: [{ ip: client }] }));

    await page.goto('/lists');
    await settle(page);
    await main(page).getByRole('button', { name: 'New list' }).click();
    const wizard = page.getByRole('dialog', { name: 'New list' });
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('switch', { name: /Add an RPZ feed/ }).click({ force: true });
    await wizard.getByRole('textbox', { name: /^Policy zone/ }).fill(zone);
    await wizard.getByRole('button', { name: 'Add primary server' }).click();
    await wizard.getByRole('textbox', { name: 'Primary servers 1' }).fill(primary.address);
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('textbox', { name: /^Name/ }).fill(name);
    // The test's own policy applies the list, to its own client only.
    for (const box of await wizard.getByRole('group', { name: 'Use in policies' }).getByRole('checkbox', { checked: true }).all()) await box.uncheck();
    await wizard.getByRole('button', { name: 'Next' }).click();
    await expect(wizard.getByRole('table', { name: 'Sources to create' })).toContainText(`${zone} from ${primary.address}`);
    await wizard.getByRole('button', { name: 'Create list' }).click();
    await expect(page).toHaveURL((url) => url.pathname === `/lists/default/${name}`);
    await api.put(`/objects/policies/default/${name}`, object('Policy', name, { clientGroupRefs: [name], blockListRefs: [name], priority: 500 }));

    await expect.poll(feedState, { timeout: 30_000, intervals: [250, 500, 1000] }).toBe('serving');
    await expect.poll(async () => (await dig(blocked)).status_class, { timeout: 20_000, intervals: [250, 500, 1000] }).toBe('blocked');
    expect(primary.transfers()).toBeGreaterThan(0);

    await page.reload();
    await settle(page);
    const region = main(page).getByRole('region', { name: 'RPZ feeds on this node' });
    await expect(region).toContainText(`node ${env('KUDURRU_E2E_NODE')}`);
    const feed = region.locator('[data-state]').first();
    await expect(feed).toHaveAttribute('data-state', 'serving');
    await expect(feed).toContainText(primary.address);
    await expect(feed).toContainText(/Serial\s*7(?!\d)/);
    await expect(feed.getByTestId('rpz-rules')).toContainText(/QNAME\s*1(?!\d)/);
    await expect(feed.getByRole('table', { name: /^Ignored owners of / })).toContainText(`ns.example.rpz-nsdname.${zone}`);
    await expect(feed.getByRole('alert')).toHaveCount(0);
    await expect(page.getByTestId(/^source-/).first()).toContainText(zone);
    await page.screenshot({ path: path.join(screenshotDir, 'rpz-ui-serving.png'), fullPage: true });

    // The primary stops answering: the copy expires 8 s after its last confirmation.
    primary.fail();
    await expect.poll(feedState, { timeout: 40_000, intervals: [500, 1000] }).toBe('expired');
    await expect.poll(async () => (await dig(blocked)).rcode, { timeout: 20_000, intervals: [250, 500, 1000] }).toBe('SERVFAIL');
    await page.reload();
    await settle(page);
    await expect(feed).toHaveAttribute('data-state', 'expired');
    await expect(feed.getByRole('alert')).toContainText('Expired: clients get SERVFAIL');
    await expect(feed.getByRole('alert')).toContainText('RPZExpired');
    await expect(feed).toContainText(/Last serial\s*7(?!\d)/);
    await page.screenshot({ path: path.join(screenshotDir, 'rpz-ui-expired.png'), fullPage: true });

    await page.goto('/lists');
    await settle(page);
    await expect(main(page).getByRole('row', { name: new RegExp(name) }).getByTestId('rpz-state')).toHaveText('RPZ expired', { timeout: 35_000 });
  } finally {
    await api.del(`/objects/policies/default/${name}`);
    await api.del(`/objects/blocklists/default/${name}`);
    await api.del(`/objects/clientgroups/default/${name}`);
    await primary.close();
  }
});
