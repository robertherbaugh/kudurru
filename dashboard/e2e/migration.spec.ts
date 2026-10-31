import fs from 'node:fs';
import path from 'node:path';
import type { DNSZone, MigrationResult, MigrationRun } from '../src/api/types';
import { expect, settle, test } from './fixtures';
import { adminApi, suffix } from './support/api';

test('migration history cuts over served NS and SOA through the dashboard', async ({ page }) => {
  const api = await adminApi(page);
  const name = `migration-${suffix()}`;
  const domain = `${name}.test.`;
  const result = await api.post<MigrationResult>('/imports/apply', {
    source: { type: 'technitium', url: 'https://source.test' }, warnings: [], settings: {},
    objects: [{ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { zoneName: domain, type: 'primary', nameservers: ['old.test.'], soa: { mname: 'old.test.', rname: 'hostmaster.test.' } } }],
  });
  expect(result.items[0]?.result).toBe('created');
  try {
    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: `Review migration ${result.runID}` }).click();
    await page.getByRole('button', { name: 'Decommission source nameservers' }).click();
    const dialog = page.getByRole('dialog', { name: 'Decommission source nameservers' });
    await dialog.getByRole('combobox', { name: 'Advertise kudurru using' }).click();
    await page.getByRole('option', { name: 'DNS names', exact: true }).click();
    await dialog.getByPlaceholder('dns1.example.com').fill('dns1.kudurru.test');
    await dialog.getByRole('button', { name: 'Add nameserver' }).click();
    await dialog.getByPlaceholder('dns1.example.com').nth(1).fill('dns2.kudurru.test');
    await dialog.getByRole('button', { name: 'Next' }).click();
    await expect(dialog.getByRole('table', { name: 'Nameserver changes' })).toContainText('old.test.');
    await expect(dialog).toContainText('dns1.kudurru.test.');
    const screenshots = path.resolve('..', '.context'); fs.mkdirSync(screenshots, { recursive: true });
    await page.screenshot({ path: path.join(screenshots, 'migration-cutover.png') });
    await dialog.getByRole('checkbox', { name: 'I reviewed the nameserver changes' }).check();
    await dialog.getByRole('button', { name: 'Update nameservers' }).click();
    await expect(page.getByText('Nameservers updated', { exact: true })).toBeVisible();
    const zone = await api.get<DNSZone>(`/objects/dnszones/${name}`);
    expect(zone.body.spec.nameservers).toEqual(['dns1.kudurru.test.', 'dns2.kudurru.test.']);
    expect(zone.body.spec.soa?.mname).toBe('dns1.kudurru.test.');
    const run = await api.get<MigrationRun>(`/imports/runs/${result.runID}`);
    expect(run.body.data.cutovers[0]?.status).toBe('completed');
    for (const type of ['NS', 'SOA']) {
      await expect.poll(async () => {
        const dns = await api.post<{ answer: string[] }>('/dns/query', { name: domain, type });
        return dns.answer.join('\n');
      }).toContain('dns1.kudurru.test.');
    }
  } finally { await api.del(`/zones/${name}`); }
});


test('VIP cutover creates and serves generated nameserver addresses', async ({ page, pendingReadPaths }) => {
  const api = await adminApi(page);
  const name = `migration-vip-${suffix()}`;
  const domain = `${name}.test.`;
  const result = await api.post<MigrationResult>('/imports/apply', {
    source: { type: 'technitium', url: 'https://source.test' }, warnings: [], settings: {},
    objects: [{ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { zoneName: domain, type: 'primary', nameservers: ['old.test.'], soa: { mname: 'old.test.', rname: 'hostmaster.test.' } } }],
  });
  try {
    // Finish the seed primary's status publication before reviewing its RV.
    // A later Ready status update correctly invalidates an earlier cutover plan.
    await expect.poll(async () => {
      const zone = await api.get<DNSZone>(`/objects/dnszones/${name}`);
      if (zone.status !== 200) return false;
      const { metadata, status } = zone.body;
      return status?.observedGeneration === metadata.generation && status?.conditions?.some((c) => c.type === 'Ready' && c.status === 'True' && c.observedGeneration === metadata.generation) === true;
    }).toBe(true);
    await page.route('**/api/v1/edge', (route) => route.fulfill({ json: { vips: [{ address: '192.0.2.53/24' }, { address: '2001:db8::53/64' }] } }));
    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: `Review migration ${result.runID}` }).click();
    await page.getByRole('button', { name: 'Decommission source nameservers' }).click();
    const dialog = page.getByRole('dialog', { name: 'Decommission source nameservers' });
    await dialog.getByRole('button', { name: 'Use detected VIPs' }).click();
    await dialog.getByRole('button', { name: 'Next' }).click();
    const ns = `ns-c0000235.${domain}`;
    await expect(dialog.getByRole('table', { name: 'Generated nameserver addresses' })).toContainText(ns);
    await expect(dialog.getByRole('table', { name: 'Generated nameserver addresses' })).toContainText('A 192.0.2.53');
    await page.screenshot({ path: path.resolve('..', '.context', 'migration-vip-cutover.png') });
    // Cutover commits before the complete serving publication. A subsequent
    // read can receive the explicit pre-handler generation_pending refusal.
    // The UI must recover by reading the committed report; never replay cutover.
    pendingReadPaths.add(`/api/v1/imports/runs/${result.runID}`);
    let pendingInjected = false;
    await page.route(new URL(`/api/v1/imports/runs/${result.runID}`, page.url()).href, async (route) => {
      if (route.request().method() !== 'GET' || pendingInjected) return route.continue();
      pendingInjected = true;
      // Deterministically exercise the same pre-handler refusal seen in CI;
      // every subsequent read reaches the real server and its committed journal.
      await route.fulfill({ status: 503, contentType: 'application/json', headers: { 'Retry-After': '1' }, json: {
        error: { key: 'generation_pending', message: 'complete serving generation unavailable' }, took_ms: 0,
      } });
    });
    let cutoverRequests = 0;
    page.on('request', (request) => {
      if (request.method() === 'POST' && new URL(request.url()).pathname === `/api/v1/imports/runs/${result.runID}/decommission`) cutoverRequests++;
    });
    await dialog.getByRole('checkbox', { name: 'I reviewed the nameserver changes' }).check();
    await dialog.getByRole('button', { name: 'Update nameservers' }).click();
    await expect(page.getByText('Nameservers updated', { exact: true })).toBeVisible();
    await expect(page.getByText('Cutover 1: completed', { exact: true })).toBeVisible();
    expect(pendingInjected, 'exercise publication refusal before report recovery').toBe(true);
    expect(cutoverRequests, 'the committed cutover must never be replayed').toBe(1);
    for (const [record, type, target] of [[domain, 'NS', ns], [domain, 'SOA', ns], [ns, 'A', '192.0.2.53'], [`ns-20010db8000000000000000000000053.${domain}`, 'AAAA', '2001:db8::53']]) {
      await expect.poll(async () => {
        const dns = await api.post<{ answer: string[] }>('/dns/query', { name: record, type });
        return dns.answer.join('\n');
      }).toContain(target);
    }
  } finally { await api.del(`/zones/${name}`); }
});

test('wizard starts a selected background import with a durable partial-success report', async ({ page }) => {
  const api = await adminApi(page);
  const name = `background-${suffix()}`;
  const skipped = `${name}-skip`;
  await page.route('**/api/v1/imports/preview', (route) => route.fulfill({ json: {
    source: { type: 'technitium', url: 'https://source.test' }, warnings: ['Review DNS answers before switching clients.'],
    objects: [
      { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { type: 'primary', zoneName: `${name}.test.`, nameservers: ['old.test.'], soa: { mname: 'old.test.', rname: 'hostmaster.test.' } } },
      { apiVersion: 'kudurru.io/v1alpha1', kind: 'BlockList', metadata: { name: 'e2e-ads', namespace: 'default' }, spec: { mode: 'block', sources: [{ name: 'migrated', entries: [{ value: 'example.test', type: 'exact' }] }] } },
      { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: skipped }, spec: { type: 'primary', zoneName: `${skipped}.test.` } },
    ], settings: { blocking: { enabled: false } },
  } }));
  try {
    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: 'Import from DNS server', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Import from DNS server' });
    await dialog.getByLabel('Server URL').fill('https://source.test');
    await dialog.getByLabel('API token').fill('transient-source-token');
    await dialog.getByRole('button', { name: 'Next' }).click();
    await dialog.getByRole('checkbox', { name: `Import DNSZone/${skipped}`, exact: true }).uncheck();
    await expect(dialog).toContainText('2 of 3 objects and 1 settings section selected.');
    await page.screenshot({ path: path.resolve('..', '.context', 'migration-selection.png') });
    await dialog.getByRole('checkbox', { name: 'I reviewed the configuration and migration warnings' }).check();
    const started = page.waitForResponse((r) => r.url().endsWith('/api/v1/imports/start') && r.request().method() === 'POST');
    await dialog.getByRole('button', { name: 'Import configuration' }).click();
    const response = await started;
    expect(response.status()).toBe(202);
    expect(response.request().postData()).not.toContain('transient-source-token');
    const run = await response.json() as MigrationRun;
    await expect(page).toHaveURL(new RegExp(`/tools/migrations/${run.id}$`));
    await expect(dialog).not.toBeVisible();
    await expect(page.getByText('Import completed with errors', { exact: true })).toBeVisible();
    const results = page.getByRole('table', { name: 'Import results', exact: true });
    await expect(results).toContainText('Created');
    await expect(results).toContainText('Failed');
    await expect(results).toContainText('Not selected');
    await expect(results).toContainText('Not applied');
    await expect(results).toContainText(/managed by/i);
    expect((await api.get(`/objects/dnszones/${name}`)).status).toBe(200);
    expect((await api.get(`/objects/dnszones/${skipped}`)).status).toBe(404);
    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: `Review migration ${run.id}`, exact: true }).click();
    await page.reload(); await settle(page);
    await expect(page.getByText('Import completed with errors', { exact: true })).toBeVisible();
    await expect(page.getByText(/1 succeeded, 1 failed, 0 unconfirmed, 1 not applied, 1 not selected/)).toBeVisible();
    await page.setViewportSize({ width: 1280, height: 1400 });
    await page.screenshot({ path: path.resolve('..', '.context', 'migration-results.png'), fullPage: true });
  } finally { await api.del(`/zones/${name}`); }
});
