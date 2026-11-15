import { createHash } from 'node:crypto';
import type { DNSRecordSet, DNSZone, MigrationResult, MigrationRun } from '../src/api/types';
import { expect, settle, test } from './fixtures';
import { adminApi, suffix } from './support/api';

test('tracking a previous import preserves records and enables a reviewed authority cutover', async ({ page }) => {
  const api = await adminApi(page);
  const domain = `adoption-${suffix()}.test.`;
  // Persisted imports from before migration journals used this stable identity.
  const slug = domain.replace(/[^a-z0-9-]+/g, '-').replace(/^-|-$/g, '');
  const name = `technitium-${slug}-${createHash('sha256').update(domain).digest('hex').slice(0, 8)}`;
  const recordName = `${name}-records`;
  await api.put(`/objects/dnszones/${name}`, {
    apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name },
    spec: { type: 'primary', zoneName: domain, nameservers: ['old.test.'], soa: { mname: 'old.test.', rname: 'hostmaster.test.' } },
  });
  try {
    await api.put(`/objects/dnsrecordsets/default/${recordName}`, {
      apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: recordName, namespace: 'default' },
      spec: { zoneRef: { name }, endpoints: [{ dnsName: `www.${domain}`, recordType: 'A', recordTTL: 300, targets: ['192.0.2.82'] }] },
    });
    const beforeZone = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    const beforeRecords = (await api.get<DNSRecordSet>(`/objects/dnsrecordsets/default/${recordName}`)).body;
    await expect.poll(async () => (await api.post<{ answer: string[] }>('/dns/query', { name: `www.${domain}`, type: 'A' })).answer.join('\n')).toContain('192.0.2.82');

    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: 'Track previous imports', exact: true }).click();
    const tracking = page.getByRole('dialog', { name: 'Track previous imports', exact: true });
    await tracking.getByRole('checkbox', { name: `Track ${domain}`, exact: true }).check();
    const adopted = page.waitForResponse((r) => r.url().endsWith('/api/v1/imports/adopt') && r.request().method() === 'POST');
    await tracking.getByRole('button', { name: 'Track selected zones', exact: true }).click();
    const response = await adopted;
    expect(response.status()).toBe(200);
    const result = await response.json() as MigrationResult;
    expect(result.items).toEqual([expect.objectContaining({ kind: 'DNSZone', name, result: 'updated' })]);
    await expect(tracking).not.toBeVisible();

    await page.getByRole('button', { name: `Review migration ${result.runID}`, exact: true }).click();
    await expect(page.getByText('Import completed', { exact: true })).toBeVisible();
    await page.reload(); await settle(page);
    const run = (await api.get<MigrationRun>(`/imports/runs/${result.runID}`)).body;
    expect(run.data.source).toBe('existing');
    expect(run.data.status).toBe('completed');
    const tracked = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    expect(tracked.metadata.uid).toBe(beforeZone.metadata.uid);
    expect(tracked.spec).toEqual(beforeZone.spec);
    expect(tracked.metadata.annotations?.['kudurru.io/migration-run']).toBe(result.runID);
    const recordIdentity = (record: DNSRecordSet) => ({ uid: record.metadata.uid, generation: record.metadata.generation, spec: record.spec });
    expect(recordIdentity((await api.get<DNSRecordSet>(`/objects/dnsrecordsets/default/${recordName}`)).body)).toEqual(recordIdentity(beforeRecords));

    await page.getByRole('button', { name: 'Decommission source nameservers', exact: true }).click();
    const cutover = page.getByRole('dialog', { name: 'Decommission source nameservers', exact: true });
    await cutover.getByRole('combobox', { name: 'Advertise kudurru using' }).click();
    await page.getByRole('option', { name: 'DNS names', exact: true }).click();
    await cutover.getByPlaceholder('dns1.example.com').fill('dns1.kudurru.test');
    await cutover.getByRole('button', { name: 'Next', exact: true }).click();
    await expect(cutover.getByRole('table', { name: 'Nameserver changes' })).toContainText('old.test.');
    await cutover.getByRole('checkbox', { name: 'I reviewed the nameserver changes' }).check();
    await cutover.getByRole('button', { name: 'Update nameservers', exact: true }).click();
    await expect(page.getByText('Nameservers updated', { exact: true })).toBeVisible();
    await expect.poll(async () => (await api.post<{ answer: string[] }>('/dns/query', { name: domain, type: 'NS' })).answer.join('\n')).toContain('dns1.kudurru.test.');
    expect(recordIdentity((await api.get<DNSRecordSet>(`/objects/dnsrecordsets/default/${recordName}`)).body)).toEqual(recordIdentity(beforeRecords));
    await expect.poll(async () => (await api.post<{ answer: string[] }>('/dns/query', { name: `www.${domain}`, type: 'A' })).answer.join('\n')).toContain('192.0.2.82');
  } finally { await api.del(`/zones/${name}`); }
});

test('a selected settings-only import completes durably and survives a report reload', async ({ page }) => {
  const api = await adminApi(page);
  type Settings = { settings: { blocking: { initStrategy: string } } };
  const before = (await api.get<Settings>('/settings')).body.settings.blocking.initStrategy;
  const next = before === 'failOnError' ? 'blocking' : 'failOnError';
  const me = await page.request.get('/api/v1/auth/me');
  const { csrf } = await me.json() as { csrf: string };
  await page.route('**/api/v1/imports/preview', (route) => route.fulfill({ json: {
    source: { type: 'technitium', url: 'https://source.test' }, warnings: [],
    objects: [], settings: { blocking: { initStrategy: next } },
  } }));
  try {
    await page.goto('/tools'); await settle(page);
    await page.getByRole('button', { name: 'Import from DNS server', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Import from DNS server', exact: true });
    await dialog.getByLabel('Server URL').fill('https://source.test');
    await dialog.getByLabel('API token').fill('transient-source-token');
    await dialog.getByRole('button', { name: 'Next', exact: true }).click();
    await expect(dialog).toContainText('0 of 0 objects and 1 settings section selected.');
    await dialog.getByRole('checkbox', { name: 'I reviewed the configuration and migration warnings' }).check();
    const started = page.waitForResponse((r) => r.url().endsWith('/api/v1/imports/start') && r.request().method() === 'POST');
    await dialog.getByRole('button', { name: 'Import configuration', exact: true }).click();
    const response = await started;
    expect(response.status()).toBe(202);
    expect(response.request().postData()).not.toContain('transient-source-token');
    const accepted = await response.json() as MigrationRun;
    await expect(page).toHaveURL(new RegExp(`/tools/migrations/${accepted.id}$`));
    await expect(page.getByText('Import completed', { exact: true })).toBeVisible();
    await expect(page.getByRole('table', { name: 'Import results', exact: true })).toContainText('Updated');
    await expect.poll(async () => (await api.get<Settings>('/settings')).body.settings.blocking.initStrategy).toBe(next);
    await page.reload(); await settle(page);
    await expect(page.getByText('Import completed', { exact: true })).toBeVisible();
    const run = (await api.get<MigrationRun>(`/imports/runs/${accepted.id}`)).body;
    expect(run.data.status).toBe('completed');
    expect(run.data.settingsPersisted).toBe(true);
    expect(run.data.settingsError).toBeFalsy();
    expect(run.data.items).toEqual([expect.objectContaining({ kind: 'Settings', name: 'blocking', result: 'updated', hint: 'Restart required for: blocking.initStrategy' })]);
  } finally {
    const restored = await page.request.patch('/api/v1/settings', { headers: { 'X-Kudurru-CSRF': csrf }, data: { blocking: { initStrategy: before } } });
    expect(restored.status()).toBe(200);
    await expect.poll(async () => (await api.get<Settings>('/settings')).body.settings.blocking.initStrategy).toBe(before);
  }
});
