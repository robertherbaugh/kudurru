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
