// Real browser workflow: configure a secondary of another kudurru node,
// obtain AXFR data, edit endpoints/key names and observe local expiry.
import fs from 'node:fs';
import path from 'node:path';
import { expect, expectRendered, env, main, settle, test } from './fixtures';
import { adminApi } from './support/api';
import { query } from './support/dns';
import { REPO_DIR, startServer } from './support/kudurru';
import type { DNSZone, ZoneSummary } from '../src/api/types';

test('a secondary is created, transferred, edited and expires on this node', async ({ page, playwright }) => {
  test.setTimeout(90_000);
  const name = `secondary-${Date.now().toString(36)}.example`;
  const apex = `${name}.`;
  const primary = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
    node: 'e2e-secondary-primary', upstream: env('KUDURRU_E2E_UPSTREAM'), adminPassword: 'secondary-e2e-password',
  });
  let primaryStopped = false;
  const primaryApi = await playwright.request.newContext({ baseURL: primary.url, ignoreHTTPSErrors: true });
  await page.goto('/zones');
  const api = await adminApi(page);
  const screenshotDir = path.join(REPO_DIR, '.context');
  fs.mkdirSync(screenshotDir, { recursive: true });
  try {
    expect((await primaryApi.post('/api/v1/auth/login', { data: { username: 'admin', password: primary.adminPassword } })).ok()).toBe(true);
    const { csrf } = await (await primaryApi.get('/api/v1/auth/me')).json() as { csrf: string };
    const headers = { 'X-Kudurru-CSRF': csrf };
    expect((await primaryApi.put(`/api/v1/objects/dnszones/${name}`, { headers, data: {
      apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name },
      spec: { zoneName: apex, type: 'primary', nameservers: [`ns1.${apex}`], policy: { zoneTransfer: 'allowOnlyPrivate' }, soa: { mname: `ns1.${apex}`, refresh: 1, retry: 1, expire: 5 } },
    } })).ok()).toBe(true);
    expect((await primaryApi.post(`/api/v1/zones/${name}/records`, { headers, data: { dnsName: `www.${name}`, recordType: 'A', targets: ['192.0.2.77'] } })).ok()).toBe(true);
    await expect.poll(async () => (await query(primary.dns, `www.${name}`)).a).toContain('192.0.2.77');

    await main(page).getByRole('button', { name: 'New zone' }).click();
    const wizard = page.getByRole('dialog', { name: 'New zone' });
    await wizard.getByRole('radio', { name: /^Secondary/ }).click();
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('textbox', { name: /^Zone name/ }).fill(apex);
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('button', { name: 'Add primary server' }).click();
    const hostnameEndpoint = primary.dns.replace('127.0.0.1', 'localhost');
    await wizard.getByRole('textbox', { name: 'Primary servers 1' }).fill(hostnameEndpoint);
    await wizard.getByRole('button', { name: 'Next' }).click();
    await expect(wizard.getByTestId('zone-review')).toContainText('AXFR/IXFR over TCP');
    await page.screenshot({ path: path.join(screenshotDir, 'secondary-ui-wizard.png'), fullPage: true });
    await wizard.getByRole('button', { name: 'Create zone' }).click();
    await expect(page).toHaveURL((url) => url.pathname === `/zones/${name}`);

    const readRuntime = async () => (await api.get<ZoneSummary>(`/zones/${name}`)).body.secondaryRuntime;
    await expect.poll(async () => (await readRuntime())?.state, { timeout: 30_000 }).toBe('serving');
    await expect.poll(async () => (await query(env('KUDURRU_E2E_DNS'), `www.${name}`)).a).toContain('192.0.2.77');
    await page.reload();
    await settle(page);
    await expectRendered(page);
    const runtime = main(page).getByRole('region', { name: 'Secondary on this node' });
    await expect(runtime).toContainText(`on node ${env('KUDURRU_E2E_NODE')}`);
    await expect(runtime).toContainText('Served serial');
    await expect(runtime).toContainText('Expires at');
    for (const action of ['Add record', 'Import BIND', 'Export', 'Send NOTIFY']) await expect(main(page).getByRole('button', { name: action })).toHaveCount(0);
    await page.screenshot({ path: path.join(screenshotDir, 'secondary-ui-serving.png'), fullPage: true });

    // Preserve an unexposed policy while changing the endpoint and key in the UI.
    const existing = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    await api.put(`/objects/dnszones/${name}`, {
      apiVersion: existing.apiVersion, kind: existing.kind, metadata: existing.metadata,
      spec: { ...existing.spec, policy: { ...existing.spec.policy, queryAccess: 'allowOnlyPrivate' } },
    });
    await page.reload();
    await settle(page);
    const settings = main(page).getByTestId('section-zone-settings');
    await settings.getByRole('button', { name: 'Edit Zone settings' }).click();
    await settings.getByRole('textbox', { name: 'Primary servers 1' }).fill(primary.dns);
    await settings.getByRole('textbox', { name: /^TSIG key name/ }).fill('missing-key.example.');
    await settings.getByRole('button', { name: 'Save Zone settings' }).click();
    await expect(settings.getByRole('button', { name: 'Save Zone settings' })).toHaveCount(0);
    let saved = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    expect(saved.spec.secondary).toEqual({ primaries: [primary.dns], transferProtocol: 'tcp', tsigKeyRef: 'missing-key.example.' });
    expect(saved.spec.policy?.queryAccess).toBe('allowOnlyPrivate');
    await expect.poll(async () => (await readRuntime())?.state).toBe('unavailable');
    await settings.getByRole('button', { name: 'Edit Zone settings' }).click();
    await settings.getByRole('textbox', { name: /^TSIG key name/ }).fill('');
    await settings.getByRole('button', { name: 'Save Zone settings' }).click();
    await expect(settings.getByRole('button', { name: 'Save Zone settings' })).toHaveCount(0);
    saved = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    expect(saved.spec.secondary?.tsigKeyRef).toBeUndefined();
    expect(saved.spec.policy?.queryAccess).toBe('allowOnlyPrivate');
    await expect.poll(async () => (await readRuntime())?.state, { timeout: 30_000 }).toBe('serving');

    await primary.stop();
    primaryStopped = true;
    await expect.poll(async () => (await readRuntime())?.state, { timeout: 15_000 }).toBe('expired');
    expect((await query(env('KUDURRU_E2E_DNS'), `www.${name}`)).rcode).toBe(2);
    await page.reload();
    await settle(page);
    await expect(runtime).toContainText('Expired');
    await expect(runtime).toContainText('Last snapshot serial');
    await expect(runtime.getByText('Served serial', { exact: true })).toHaveCount(0);
    await page.screenshot({ path: path.join(screenshotDir, 'secondary-ui-expired.png'), fullPage: true });
    await page.goto('/zones');
    const row = main(page).getByRole('row').filter({ has: page.getByRole('link', { name: apex, exact: true }) });
    await expect(row).toContainText(`on node ${env('KUDURRU_E2E_NODE')}`);
    await expect(row).toContainText('Expired');
    await expect(row).toContainText('last snapshot');
  } finally {
    // Leave the detail page before deleting so its periodic reads cannot race cleanup.
    await page.goto('/zones');
    await api.del(`/zones/${name}`);
    await primaryApi.dispose();
    if (!primaryStopped) await primary.stop();
  }
});
