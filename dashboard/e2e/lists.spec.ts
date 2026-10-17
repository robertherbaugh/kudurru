// Lists: the new-list wizard, the per-source enable switch (a disabled
// source stops blocking, checked with Tools → Query as a client of the
// test's own group and policy) and the inline entries browser. The last two
// need ux-plan C2 on the server (spec.sources[].enabled and
// /blocklists/{ns}/{name}/sources/{source}/entries) and skip without it.
import type { Page } from '@playwright/test';
import { expect, settle, test } from './fixtures';
import { adminApi, hasListSourcesAPI, object, suffix, type Api } from './support/api';

/** Polls POST /dns/query until name resolves blocked (or not) for client. */
async function waitBlocked(api: Api, name: string, client: string, blocked: boolean): Promise<void> {
  await expect
    .poll(
      async () => (await api.post<{ status_class: string }>('/dns/query', { name, type: 'A', client })).status_class === 'blocked',
      { message: `${name} as ${client} ${blocked ? 'blocked' : 'not blocked'}`, timeout: 20_000, intervals: [250, 500, 1000] },
    )
    .toBe(blocked);
}

/** A toast: the text also lands in the polite live region, so scope it. */
function toast(page: Page, text: string) {
  return page.getByRole('region', { name: 'Notifications' }).getByText(text);
}

/** Tools → Query (dig) as client; returns the result region. */
async function dig(page: Page, name: string, client: string) {
  await page.goto('/tools');
  await settle(page);
  const panel = page.getByRole('region', { name: 'Query (dig)' });
  await panel.getByRole('textbox', { name: 'Name' }).fill(name);
  await panel.getByRole('textbox', { name: /^As client/ }).fill(client);
  await panel.getByRole('button', { name: 'Query' }).click();
  const result = panel.locator('[aria-live="polite"]').last();
  await expect(result).toContainText(name);
  return result;
}

test('the wizard creates a list from pasted entries', async ({ page }) => {
  const api = await adminApi(page);
  const id = suffix();
  const name = `e2e-wizard-${id}`;
  try {
    await page.goto('/lists');
    await settle(page);
    await page.getByRole('button', { name: 'New list' }).click();
    await expect(page).toHaveURL(/\/lists\?new=1$/);
    await expect(page.getByRole('dialog', { name: 'New list' })).toBeVisible();
    await expect(page.getByRole('radio', { name: /Block list/ })).toBeChecked();
    await page.getByRole('button', { name: 'Next' }).click();
    await page.getByRole('textbox', { name: /^Paste entries/ }).fill(`wizard-${id}.example\nother-${id}.example`);
    await page.getByRole('button', { name: 'Next' }).click();
    await page.getByRole('textbox', { name: /^Name/ }).fill(name);
    // Leave the policies alone: this list is only checked, not applied.
    for (const box of await page.getByRole('group', { name: 'Use in policies' }).getByRole('checkbox', { checked: true }).all()) await box.uncheck();
    await page.getByRole('button', { name: 'Next' }).click();
    await expect(page.getByRole('table', { name: 'Sources to create' })).toContainText('2 inline entries');
    await page.getByRole('button', { name: 'Create list' }).click();

    await expect(page).toHaveURL(new RegExp(`/lists/default/${name}$`));
    await expect(page.getByRole('heading', { level: 1, name })).toBeVisible();
    const row = page.getByTestId('source-custom');
    await expect(row).toContainText('2 inline entries');
    await expect(row.getByRole('switch', { name: 'Source custom enabled' })).toBeChecked();
    const stored = await api.get<{ spec: { mode: string; sources: { name: string; inline: string[] }[] } }>(`/objects/blocklists/default/${name}`);
    expect(stored.body.spec.mode).toBe('block');
    expect(stored.body.spec.sources[0]?.inline).toEqual([`wizard-${id}.example`, `other-${id}.example`]);
  } finally {
    await api.del(`/objects/blocklists/default/${name}`);
  }
});

test('turning a source off stops it blocking (Tools → Query)', async ({ page }) => {
  const api = await adminApi(page);
  test.skip(!(await hasListSourcesAPI(api)), 'needs ux-plan C2 on the server: spec.sources[].enabled');
  const id = suffix();
  const name = `e2e-toggle-${id}`;
  const client = `198.51.100.${10 + Math.floor(Math.random() * 200)}`;
  const kept = `kept-${id}.example`;
  const toggled = `toggled-${id}.example`;
  try {
    await api.put(`/objects/clientgroups/default/${name}`, object('ClientGroup', name, { clients: [{ ip: client }] }));
    await api.put(`/objects/blocklists/default/${name}`, object('BlockList', name, { mode: 'block', sources: [{ name: 'keep', inline: [kept] }, { name: 'toggle', inline: [toggled] }] }));
    await api.put(`/objects/policies/default/${name}`, object('Policy', name, { clientGroupRefs: [name], blockListRefs: [name], priority: 500 }));
    await waitBlocked(api, toggled, client, true);

    await page.goto(`/lists/default/${name}`);
    await settle(page);
    const sw = page.getByTestId('source-toggle').getByRole('switch', { name: 'Source toggle enabled' });
    await expect(sw).toBeChecked();
    await sw.click({ force: true });
    await expect(toast(page, 'toggle disabled')).toBeVisible();
    await expect(sw).not.toBeChecked();
    await expect(page.getByText('1 of 2 enabled')).toBeVisible();

    await waitBlocked(api, toggled, client, false);
    const off = await dig(page, toggled, client);
    await expect(off).not.toContainText(/blocked/i);
    const on = await dig(page, kept, client);
    await expect(on).toContainText(/blocked/i);
    await expect(on).toContainText(name);
  } finally {
    await api.del(`/objects/policies/default/${name}`);
    await api.del(`/objects/blocklists/default/${name}`);
    await api.del(`/objects/clientgroups/default/${name}`);
  }
});

test('the entries browser pages, adds and removes inline entries', async ({ page }) => {
  const api = await adminApi(page);
  test.skip(!(await hasListSourcesAPI(api)), 'needs ux-plan C2 on the server: the inline entries API');
  const id = suffix();
  const name = `e2e-entries-${id}`;
  const entries = Array.from({ length: 60 }, (_, i) => `e${String(i).padStart(2, '0')}-${id}.example`);
  const added = `added-${id}.example`;
  try {
    await api.put(`/objects/blocklists/default/${name}`, object('BlockList', name, { mode: 'block', sources: [{ name: 'mine', format: 'domains', inline: entries }] }));
    await page.goto(`/lists/default/${name}`);
    await settle(page);
    await page.getByRole('button', { name: 'Browse the 60 inline entries of mine' }).click();
    const browser = page.getByTestId('entries-browser');
    await expect(browser.getByText('Showing 1–25 of 60')).toBeVisible();
    await browser.getByRole('button', { name: 'Next page' }).click();
    await expect(browser.getByText('Showing 26–50 of 60')).toBeVisible();

    await browser.getByRole('textbox', { name: /^Add entries/ }).fill(added);
    await browser.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(toast(page, 'Added 1 entry to mine')).toBeVisible();
    await browser.getByRole('textbox', { name: /^Search entries/ }).fill(`added-${id}`);
    const row = browser.getByRole('row').filter({ hasText: added });
    await expect(row).toBeVisible();
    await row.getByRole('checkbox').check({ force: true });
    await browser.getByRole('button', { name: 'Remove 1 selected' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Remove' }).click();
    await expect(toast(page, 'Removed 1 entry from mine')).toBeVisible();
    await expect(browser.getByText(`No entry contains "added-${id}"`)).toBeVisible();

    const stored = await api.get<{ spec: { sources: { inline: string[] }[] } }>(`/objects/blocklists/default/${name}`);
    expect(stored.body.spec.sources[0]?.inline).toEqual(entries);
  } finally {
    await api.del(`/objects/blocklists/default/${name}`);
  }
});
