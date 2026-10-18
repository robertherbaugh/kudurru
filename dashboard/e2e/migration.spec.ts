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


test('VIP cutover creates and serves generated nameserver addresses', async ({ page }) => {
  const api = await adminApi(page);
  const name = `migration-vip-${suffix()}`;
  const domain = `${name}.test.`;
  const result = await api.post<MigrationResult>('/imports/apply', {
    source: { type: 'technitium', url: 'https://source.test' }, warnings: [], settings: {},
    objects: [{ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { zoneName: domain, type: 'primary', nameservers: ['old.test.'], soa: { mname: 'old.test.', rname: 'hostmaster.test.' } } }],
  });
  try {
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
    await dialog.getByRole('checkbox', { name: 'I reviewed the nameserver changes' }).check();
    await dialog.getByRole('button', { name: 'Update nameservers' }).click();
    await expect(page.getByText('Nameservers updated', { exact: true })).toBeVisible();
    for (const [record, type, target] of [[domain, 'NS', ns], [domain, 'SOA', ns], [ns, 'A', '192.0.2.53'], [`ns-20010db8000000000000000000000053.${domain}`, 'AAAA', '2001:db8::53']]) {
      await expect.poll(async () => {
        const dns = await api.post<{ answer: string[] }>('/dns/query', { name: record, type });
        return dns.answer.join('\n');
      }).toContain(target);
    }
  } finally { await api.del(`/zones/${name}`); }
});
