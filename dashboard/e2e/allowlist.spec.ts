// Allowlist, Denylist and switching single entries of an imported list
// off, end to end. Each test owns a BlockList with a url source on the
// stub publisher (support/lists.ts, one hosts list per test id), a
// ClientGroup of test-only addresses and an API-managed Policy covering
// it that uses the list; names are checked as those clients with POST
// /dns/query (Tools → Query) like lists.spec.ts. The dashboard's ui-allow
// and ui-deny lists are attached to that policy by the first write.
// Everything a test creates is removed again, entries included.
import type { Page } from '@playwright/test';
import { env, expect, selector, settle, test } from './fixtures';
import { choose } from './support/astryx';
import { adminApi, object, suffix, type Api } from './support/api';
import { BLOCKLIST } from './support/kudurru';
import { listHosts } from './support/lists';

interface DomainEntry {
  id: string;
  action: 'allow' | 'deny';
  value: string;
  type: string;
  everyone: boolean;
  groups: string[];
}

/** Polls POST /dns/query until name resolves blocked (or not) for client. */
async function waitBlocked(api: Api, name: string, client: string, blocked: boolean): Promise<void> {
  await expect
    .poll(async () => (await api.post<{ status_class: string }>('/dns/query', { name, type: 'A', client })).status_class === 'blocked', {
      message: `${name} as ${client} ${blocked ? 'blocked' : 'not blocked'}`,
      timeout: 30_000,
      intervals: [250, 500, 1000],
    })
    .toBe(blocked);
}

/** A toast: the text also lands in the polite live region, so scope it. */
function toast(page: Page, text: string | RegExp) {
  return page.getByRole('region', { name: 'Notifications' }).getByText(text).first();
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

/** The entries of an action whose value is value (GET /allowlist?q=). */
async function entriesOf(api: Api, action: 'allow' | 'deny', value: string): Promise<DomainEntry[]> {
  const r = await api.get<{ items: DomainEntry[] }>(`/${action}list?q=${encodeURIComponent(value)}&limit=1000`);
  return r.status === 200 ? r.body.items.filter((e) => e.value === value) : [];
}

async function removeEntries(api: Api, action: 'allow' | 'deny', values: string[]): Promise<void> {
  for (const v of values) for (const e of await entriesOf(api, action, v)) await api.del(`/${action}list/${encodeURIComponent(e.id)}`);
}

interface Fixture {
  id: string;
  name: string;
  /** The test client (in the test's group). */
  client: string;
  /** A second client in a second test group, under its own policy with the same list. */
  other: string;
  hosts: [string, string];
  cleanup: () => Promise<void>;
}

/** A url-source list, two groups of test clients and their policies; waits until the list blocks. */
async function setup(api: Api, opts: { alsoBlock?: string[] } = {}): Promise<Fixture> {
  const id = suffix();
  const name = `e2e-url-${id}`;
  const net = 10 + Math.floor(Math.random() * 200);
  const host = 10 + Math.floor(Math.random() * 200);
  const client = `10.77.${net}.${host}`;
  const other = `10.77.${net}.${host + 1}`;
  const hosts = listHosts(id);
  const refs = [name, ...(opts.alsoBlock ?? [])];
  const cleanup = async () => {
    await api.del(`/objects/policies/default/${name}`);
    await api.del(`/objects/policies/default/${name}-b`);
    await api.del(`/objects/clientgroups/default/${name}`);
    await api.del(`/objects/clientgroups/default/${name}-b`);
    await api.del(`/objects/blocklists/default/${name}`);
    // The group-scoped Allowlist and Denylist copies, when a test made them.
    await api.del(`/objects/blocklists/default/ui-allow-${name}`);
    await api.del(`/objects/blocklists/default/ui-deny-${name}`);
  };
  try {
    await api.put(`/objects/blocklists/default/${name}`, object('BlockList', name, { mode: 'block', sources: [{ name: 'stub', url: `${env('KUDURRU_E2E_LISTS')}/list/${id}.txt`, format: 'hosts' }] }));
    await api.put(`/objects/clientgroups/default/${name}`, object('ClientGroup', name, { clients: [{ ip: client }] }));
    await api.put(`/objects/clientgroups/default/${name}-b`, object('ClientGroup', `${name}-b`, { clients: [{ ip: other }] }));
    await api.put(`/objects/policies/default/${name}`, object('Policy', name, { clientGroupRefs: [name], blockListRefs: refs, priority: 500 }));
    await api.put(`/objects/policies/default/${name}-b`, object('Policy', `${name}-b`, { clientGroupRefs: [`${name}-b`], blockListRefs: refs, priority: 500 }));
    await waitBlocked(api, hosts[0], client, true);
    await waitBlocked(api, hosts[1], client, true);
    // The entries browser needs the body on this node.
    await expect.poll(async () => (await api.get(`/blocklists/default/${name}/sources/stub/entries?limit=1`)).status, { timeout: 30_000 }).toBe(200);
  } catch (e) {
    await cleanup();
    throw e;
  }
  return { id, name, client, other, hosts, cleanup };
}

test('switching one entry of a URL list off lets that name through, and back on blocks it again', async ({ page }) => {
  const api = await adminApi(page);
  const f = await setup(api);
  try {
    await page.goto(`/lists/default/${f.name}`);
    await settle(page);
    const source = page.getByTestId('source-stub');
    await source.getByRole('button', { name: 'Entries' }).click();
    const browser = page.getByTestId('entries-browser');
    await expect(browser).toContainText('2 rules');
    await expect(browser.getByText(/affects only this list/)).toBeVisible();
    const row = browser.getByRole('row').filter({ hasText: f.hosts[0] });
    const sw = row.getByRole('switch', { name: `${f.hosts[0]} enabled` });
    await expect(sw).toBeChecked();
    await sw.click({ force: true });
    await expect(toast(page, `${f.hosts[0]} switched off in ${f.name}`)).toBeVisible();
    await expect(sw).not.toBeChecked();

    await waitBlocked(api, f.hosts[0], f.client, false);
    await waitBlocked(api, f.hosts[1], f.client, true);
    const stored = await api.get<{ spec: { sources: { exclude?: string[] }[] } }>(`/objects/blocklists/default/${f.name}`);
    expect(stored.body.spec.sources[0]?.exclude).toEqual([f.hosts[0]]);
    await page.goto(`/lists/default/${f.name}`);
    await settle(page);
    await expect(page.getByTestId('source-stub')).toContainText('1 of 2 enabled');

    // "Show disabled only" lists it; switching it back on blocks it again.
    await page.getByTestId('source-stub').getByRole('button', { name: 'Entries' }).click();
    await browser.getByRole('switch', { name: 'Show disabled only' }).click({ force: true });
    await expect(browser.getByRole('row').filter({ hasText: f.hosts[1] })).toHaveCount(0);
    const back = browser.getByRole('row').filter({ hasText: f.hosts[0] }).getByRole('switch', { name: `${f.hosts[0]} enabled` });
    await back.click({ force: true });
    await expect(toast(page, `${f.hosts[0]} switched on in ${f.name}`)).toBeVisible();
    await waitBlocked(api, f.hosts[0], f.client, true);
    const dug = await dig(page, f.hosts[0], f.client);
    await expect(dug).toContainText(/blocked/i);
  } finally {
    await f.cleanup();
  }
});

test('an Allowlist entry for everyone lets a blocked name through', async ({ page }) => {
  const api = await adminApi(page);
  const f = await setup(api);
  const allowed = f.hosts[1];
  try {
    await page.goto('/allowlist');
    await settle(page);
    await page.getByRole('button', { name: 'Add to the Allowlist' }).click();
    const dialog = page.getByRole('dialog', { name: 'Add to the Allowlist' });
    await dialog.getByRole('textbox', { name: /^Domains/ }).fill(allowed);
    await dialog.getByRole('textbox', { name: /^Comment/ }).fill(`e2e ${f.id}`);
    await dialog.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(dialog).toBeHidden();
    await expect(toast(page, /^Added 1 to the Allowlist\./)).toBeVisible();

    const row = page.getByTestId(`entry-${allowed}`);
    await expect(row).toBeVisible();
    await expect(row).toContainText('Everyone');
    await expect(row).toContainText(`e2e ${f.id}`);
    await expect(row).toContainText(f.name);
    const [entry] = await entriesOf(api, 'allow', allowed);
    expect(entry).toMatchObject({ type: 'exact', everyone: true });

    await waitBlocked(api, allowed, f.client, false);
    const dug = await dig(page, allowed, f.client);
    await expect(dug).not.toContainText(/blocked/i);
    // The other name of the list stays blocked.
    await waitBlocked(api, f.hosts[0], f.client, true);

    // Switching the entry off blocks the name again.
    await page.goto(`/allowlist?q=${allowed}`);
    await settle(page);
    await page.getByTestId(`entry-${allowed}`).getByRole('switch', { name: `${allowed} enabled` }).click({ force: true });
    await expect(toast(page, new RegExp(`^${allowed.replace(/\./g, '\\.')} disabled\\.`))).toBeVisible();
    await waitBlocked(api, allowed, f.client, true);
  } finally {
    await removeEntries(api, 'allow', [allowed]);
    await f.cleanup();
  }
});

test('a file-managed policy without the Allowlist: Attach shows the YAML for its file', async ({ page }) => {
  // e2e-blocking (objects.d/e2e.yaml) has no allowListRefs, so once ui-allow
  // exists it is reported as unattached; the dashboard never edits it.
  const api = await adminApi(page);
  const value = `attach-${suffix()}.example`;
  await api.post('/allowlist', { values: [value], type: 'exact' });
  try {
    await page.goto('/allowlist');
    await settle(page);
    await expect(page.getByText('Not applied for e2e-blocking').first()).toBeVisible();
    await page.getByRole('button', { name: 'Attach ui-allow to e2e-blocking' }).click();
    const dialog = page.getByRole('dialog', { name: 'Attach ui-allow to e2e-blocking' });
    await expect(dialog).toContainText('e2e-blocking is defined in objects.d/e2e.yaml on the kudurru host');
    const yaml = dialog.getByTestId('attach-yaml');
    await expect(yaml).toContainText('allowListRefs:');
    await expect(yaml).toContainText('- ui-allow');
    await expect(yaml.getByRole('button', { name: /copy/i })).toBeVisible();
    await dialog.getByRole('button', { name: 'Close' }).last().click();
    await expect(dialog).toBeHidden();
  } finally {
    await removeEntries(api, 'allow', [value]);
  }
});

test('an Allowlist entry for a group lets only that group through', async ({ page }) => {
  const api = await adminApi(page);
  // The policies also use the run's file list, so ads.example.net is blocked
  // for the test clients as it is for 127.0.0.1 (policy e2e-blocking).
  const f = await setup(api, { alsoBlock: [BLOCKLIST.name] });
  const name = f.hosts[0];
  try {
    await waitBlocked(api, BLOCKLIST.blocked, f.client, true);
    for (const v of [name, BLOCKLIST.blocked]) {
      const r = await api.post<{ processed: { success: unknown[]; errors: unknown[] } }>('/allowlist', { values: [v], type: 'exact', groups: [`default/${f.name}`], comment: `e2e ${f.id}` });
      expect(r.processed.errors).toEqual([]);
    }
    await waitBlocked(api, name, f.client, false);
    await waitBlocked(api, BLOCKLIST.blocked, f.client, false);
    // The other group and 127.0.0.1 stay blocked.
    await waitBlocked(api, name, f.other, true);
    await waitBlocked(api, BLOCKLIST.blocked, '127.0.0.1', true);
    await expect(await dig(page, BLOCKLIST.blocked, f.client)).not.toContainText(/blocked/i);
    await expect(await dig(page, BLOCKLIST.blocked, '127.0.0.1')).toContainText(/blocked/i);

    // The page shows the group chip, linking to the group.
    await page.goto(`/allowlist?scope=default%2F${f.name}`);
    await settle(page);
    const row = page.getByTestId(`entry-${name}`);
    // (The list named like the group shows too: the entry overrides it.)
    await expect(row.getByRole('link', { name: f.name, exact: true, description: `Client group default/${f.name}` })).toHaveAttribute('href', `/groups/default/${f.name}`);
    await expect(row).toContainText(/Overrides block list/);
    await expect(row).not.toContainText('Everyone');
  } finally {
    await removeEntries(api, 'allow', [name, BLOCKLIST.blocked]);
    await f.cleanup();
  }
});

test('a Denylist wildcard blocks the name and its subdomains; an allow wildcard wins', async ({ page }) => {
  const api = await adminApi(page);
  const f = await setup(api);
  const base = `wild-${f.id}.example`;
  try {
    await waitBlocked(api, `cdn.${base}`, f.client, false);
    await page.goto('/denylist');
    await settle(page);
    await page.getByRole('button', { name: 'Add to the Denylist' }).click();
    const dialog = page.getByRole('dialog', { name: 'Add to the Denylist' });
    await choose(selector(dialog, /^Type/), 'Wildcard');
    await dialog.getByRole('textbox', { name: /^Domains/ }).fill(base);
    await expect(dialog.getByText(`Saved as *.${base}.`)).toBeVisible();
    await dialog.getByRole('button', { name: 'Add', exact: true }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByTestId(`entry-*.${base}`)).toContainText('wildcard');

    await waitBlocked(api, base, f.client, true);
    await waitBlocked(api, `cdn.${base}`, f.client, true);
    await expect(await dig(page, `cdn.${base}`, f.client)).toContainText(/blocked/i);

    // Allow wins over deny: an allow wildcard for the subdomain lets it through.
    await api.post('/allowlist', { values: [`*.cdn.${base}`], type: 'wildcard' });
    await waitBlocked(api, `img.cdn.${base}`, f.client, false);
    await waitBlocked(api, base, f.client, true);
  } finally {
    await removeEntries(api, 'deny', [`*.${base}`]);
    await removeEntries(api, 'allow', [`*.cdn.${base}`]);
    await f.cleanup();
  }
});

test('the Domains page shows allow and deny entries together', async ({ page }) => {
  const api = await adminApi(page);
  const id = suffix();
  const allowed = `both-allow-${id}.example`;
  const denied = `both-deny-${id}.example`;
  try {
    await api.post('/allowlist', { values: [allowed], comment: `e2e ${id}` });
    await api.post('/denylist', { values: [denied], comment: `e2e ${id}` });
    await page.goto(`/domains?q=${id}`);
    await settle(page);
    await expect(page.getByTestId(`entry-${allowed}`)).toContainText('allow');
    await expect(page.getByTestId(`entry-${denied}`)).toContainText('deny');
    await choose(selector(page, /^Action/), 'Deny');
    await expect(page.getByTestId(`entry-${allowed}`)).toHaveCount(0);
    await expect(page.getByTestId(`entry-${denied}`)).toBeVisible();
    await expect(page.getByRole('link', { name: 'Allowlist only' })).toHaveAttribute('href', '/allowlist');

    // Bulk delete from the combined page.
    await choose(selector(page, /^Action/), 'Allow and deny');
    await page.getByTestId(`entry-${allowed}`).getByRole('checkbox').check({ force: true });
    await page.getByTestId(`entry-${denied}`).getByRole('checkbox').check({ force: true });
    await page.getByRole('toolbar', { name: 'Bulk actions' }).getByRole('button', { name: 'Delete' }).click();
    await page.getByRole('dialog', { name: 'Delete 2 entries?' }).getByRole('button', { name: 'Delete' }).click();
    await expect(page.getByTestId(`entry-${allowed}`)).toHaveCount(0);
    await expect(page.getByTestId(`entry-${denied}`)).toHaveCount(0);
    expect(await entriesOf(api, 'allow', allowed)).toEqual([]);
    expect(await entriesOf(api, 'deny', denied)).toEqual([]);
  } finally {
    await removeEntries(api, 'allow', [allowed]);
    await removeEntries(api, 'deny', [denied]);
  }
});

test('the entries tables fit without scrolling sideways at 1280 and 1440 px', async ({ page }) => {
  const api = await adminApi(page);
  const id = suffix();
  const allowed = `fit-allow-${id}.example`;
  const denied = `fit-deny-${id}.example`;
  try {
    await api.post('/allowlist', { values: [allowed], comment: `a longer comment that explains why ${id} is allowed for everyone` });
    await api.post('/denylist', { values: [denied], comment: `e2e ${id}` });
    for (const width of [1280, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      for (const [path, name] of [
        [`/allowlist?q=${id}`, 'Allowlist'],
        [`/denylist?q=${id}`, 'Denylist'],
        [`/domains?q=${id}`, 'Domains'],
      ] as const) {
        await page.goto(path);
        await settle(page);
        const table = page.getByRole('table', { name });
        await expect(table.getByRole('row')).toHaveCount(name === 'Domains' ? 3 : 2);
        const [scroll, client] = await table.evaluate((t) => [t.parentElement?.scrollWidth ?? 0, t.parentElement?.clientWidth ?? 0]);
        expect(scroll, `${path} at ${width} px: the table scrolls sideways`).toBeLessThanOrEqual((client ?? 0) + 1);
        // The row actions are on screen.
        await expect(table.getByRole('button', { name: 'Delete' }).first()).toBeInViewport();
      }
    }
  } finally {
    await removeEntries(api, 'allow', [allowed]);
    await removeEntries(api, 'deny', [denied]);
  }
});

test('an entry switched off in a URL list shows on the Allowlist as a list exception; switching it there blocks it again', async ({ page }) => {
  const api = await adminApi(page);
  const f = await setup(api);
  const host = f.hosts[0];
  try {
    await page.goto(`/lists/default/${f.name}`);
    await settle(page);
    await page.getByTestId('source-stub').getByRole('button', { name: 'Entries' }).click();
    const browser = page.getByTestId('entries-browser');
    await browser.getByRole('row').filter({ hasText: host }).getByRole('switch', { name: `${host} enabled` }).click({ force: true });
    await expect(toast(page, `${host} switched off in ${f.name}`)).toBeVisible();
    await waitBlocked(api, host, f.client, false);

    await browser.getByRole('link', { name: 'Allowlist as list exceptions' }).click();
    await expect(page).toHaveURL(new RegExp(`/allowlist\\?kind=exception&scope=list%3Adefault%2F${f.name}$`));
    await settle(page);
    const row = page.getByTestId(`entry-${host}`);
    await expect(row).toContainText('exception');
    await expect(row).toContainText(`Only list${f.name}`);
    await expect(row.getByRole('link', { name: f.name, exact: true }).first()).toHaveAttribute('href', `/lists/default/${f.name}`);
    await expect(row.getByRole('button', { name: 'Edit' })).toHaveCount(0);
    // Its badges are not cut off at the default 1280 px.
    const clipped = await row.evaluate((tr) => [...tr.querySelectorAll<HTMLElement>('.astryx-token, .astryx-token *, .astryx-badge, .astryx-badge *')].filter((t) => t.scrollWidth > t.clientWidth + 1).map((t) => t.textContent));
    expect(clipped, 'truncated badges').toEqual([]);
    await expect(page.getByTestId(`entry-${f.hosts[1]}`)).toHaveCount(0);

    const sw = row.getByRole('switch', { name: `${host} switched off in ${f.name}` });
    await expect(sw).toBeChecked();
    await sw.click({ force: true });
    await expect(toast(page, new RegExp(`switched back on in ${f.name}: that list blocks it again`))).toBeVisible();
    // The row stays, switched off, so it can be switched off in the list again.
    await expect(sw).not.toBeChecked();
    await waitBlocked(api, host, f.client, true);
    await expect(await dig(page, host, f.client)).toContainText(/blocked/i);
    const stored = await api.get<{ spec: { sources: { exclude?: string[] }[] } }>(`/objects/blocklists/default/${f.name}`);
    expect(stored.body.spec.sources[0]?.exclude ?? []).toEqual([]);
  } finally {
    await f.cleanup();
  }
});

test('"Only in list" in the Allowlist add dialog switches a name off in that list only', async ({ page }) => {
  const api = await adminApi(page);
  const f = await setup(api);
  const host = f.hosts[1];
  try {
    await page.goto('/allowlist');
    await settle(page);
    await page.getByRole('button', { name: 'Add to the Allowlist' }).click();
    const dialog = page.getByRole('dialog', { name: 'Add to the Allowlist' });
    await choose(selector(dialog, /^Applies to/), 'Only in list …');
    await dialog.getByRole('textbox', { name: /^Domains/ }).fill(host);
    await choose(selector(dialog, /^Block list/), `default/${f.name}`);
    await dialog.getByRole('button', { name: 'Switch off', exact: true }).click();
    await expect(dialog).toBeHidden();
    await expect(toast(page, new RegExp(`^Switched 1 off in ${f.name} only`))).toBeVisible();

    await waitBlocked(api, host, f.client, false);
    await waitBlocked(api, f.hosts[0], f.client, true);
    await expect(await dig(page, host, f.client)).not.toContainText(/blocked/i);
    // Nothing was added to the Allowlist: the row is a list exception.
    const r = await api.get<{ items: DomainEntry[] }>(`/allowlist?q=${encodeURIComponent(host)}`);
    expect(r.body.items.filter((e) => e.value === host).map((e) => (e as DomainEntry & { kind: string }).kind)).toEqual(['exception']);
  } finally {
    await f.cleanup();
  }
});
