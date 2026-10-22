// Configure a stub through the dashboard and exercise its live authority route.
import fs from 'node:fs';
import path from 'node:path';
import { expect, expectRendered, env, main, settle, test } from './fixtures';
import { adminApi } from './support/api';
import { query } from './support/dns';
import { REPO_DIR, startServer } from './support/kudurru';
import type { DNSZone, ZoneSummary } from '../src/api/types';

test('a stub discovers authorities, routes queries, edits and expires', async ({ page, playwright }) => {
  test.setTimeout(90_000);
  const name = `stub-${Date.now().toString(36)}.example`;
  const apex = `${name}.`;
  const primary = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
    node: 'e2e-stub-authority', upstream: env('KUDURRU_E2E_UPSTREAM'), adminPassword: 'stub-e2e-password',
  });
  let stopped = false;
  const primaryApi = await playwright.request.newContext({ baseURL: primary.url, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
  await page.goto('/zones');
  const api = await adminApi(page);
  const screenshots = path.join(REPO_DIR, '.context');
  fs.mkdirSync(screenshots, { recursive: true });
  try {
    expect((await primaryApi.post('/api/v1/auth/login', { data: { username: 'admin', password: primary.adminPassword } })).ok()).toBe(true);
    const { csrf } = await (await primaryApi.get('/api/v1/auth/me')).json() as { csrf: string };
    const headers = { 'X-Kudurru-CSRF': csrf };
    expect((await primaryApi.put(`/api/v1/objects/dnszones/${name}`, { headers, data: {
      apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name },
      spec: { zoneName: apex, type: 'primary', nameservers: [`ns.${apex}`], soa: { mname: `ns.${apex}`, refresh: 1, retry: 1, expire: 5 } },
    } })).ok()).toBe(true);
    for (const [host, target] of [['ns', '127.0.0.1'], ['www', '192.0.2.81']]) {
      expect((await primaryApi.post(`/api/v1/zones/${name}/records`, { headers, data: { dnsName: `${host}.${name}`, recordType: 'A', targets: [target] } })).ok()).toBe(true);
    }
    await expect.poll(async () => (await query(primary.dns, `www.${name}`)).a).toContain('192.0.2.81');
    await main(page).getByRole('button', { name: 'New zone' }).click();
    const wizard = page.getByRole('dialog', { name: 'New zone' });
    await wizard.getByRole('radio', { name: /^Stub/ }).click();
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('textbox', { name: /^Zone name/ }).fill(apex);
    await wizard.getByRole('button', { name: 'Next' }).click();
    await wizard.getByRole('button', { name: 'Add bootstrap server' }).click();
    await wizard.getByRole('textbox', { name: 'Bootstrap servers 1' }).fill(`udp://${primary.dns}`);
    await wizard.getByRole('button', { name: 'Next' }).click();
    await expect(wizard.getByTestId('zone-review')).toContainText('Stub (discovered authorities)');
    await page.screenshot({ path: path.join(screenshots, 'stub-ui-wizard.png'), fullPage: true });
    await wizard.getByRole('button', { name: 'Create zone' }).click();
    await expect(page).toHaveURL((url) => url.pathname === `/zones/${name}`);
    const readRuntime = async () => (await api.get<ZoneSummary>(`/zones/${name}`)).body.stubRuntime;
    await expect.poll(async () => (await readRuntime())?.state, { timeout: 30_000 }).toBe('routing');
    await expect.poll(async () => (await query(env('KUDURRU_E2E_DNS'), `www.${name}`)).a).toContain('192.0.2.81');
    expect((await readRuntime())?.authorities).toEqual([{ name: `ns.${apex}`, address: primary.dns, protocol: 'udp' }]);
    await page.reload();
    await settle(page);
    await expectRendered(page);
    const runtime = main(page).getByRole('region', { name: 'Stub on this node' });
    await expect(runtime).toContainText(`on node ${env('KUDURRU_E2E_NODE')}`);
    await expect(runtime).toContainText('Last observed SOA serial');
    for (const action of ['Add record', 'Import BIND', 'Export', 'Send NOTIFY']) await expect(main(page).getByRole('button', { name: action })).toHaveCount(0);
    await page.screenshot({ path: path.join(screenshots, 'stub-ui-routing.png'), fullPage: true });
    const settings = main(page).getByTestId('section-zone-settings');
    await settings.getByRole('button', { name: 'Edit Zone settings' }).click();
    await settings.getByRole('textbox', { name: 'Bootstrap servers 1' }).fill(`tcp://${primary.dns}`);
    await settings.getByRole('button', { name: 'Save Zone settings' }).click();
    await expect(settings.getByRole('button', { name: 'Save Zone settings' })).toHaveCount(0);
    expect((await api.get<DNSZone>(`/objects/dnszones/${name}`)).body.spec.stub).toEqual({ nameservers: [`tcp://${primary.dns}`] });
    await expect.poll(async () => (await readRuntime())?.authorities[0]?.protocol, { timeout: 30_000 }).toBe('tcp');
    await expect.poll(async () => (await query(env('KUDURRU_E2E_DNS'), `www.${name}`)).a).toContain('192.0.2.81');
    await primary.stop();
    stopped = true;
    await expect.poll(async () => (await readRuntime())?.state, { timeout: 15_000 }).toBe('expired');
    expect((await query(env('KUDURRU_E2E_DNS'), `www.${name}`)).rcode).toBe(2);
    await page.reload();
    await settle(page);
    await expect(runtime).toContainText('Expired');
    await page.screenshot({ path: path.join(screenshots, 'stub-ui-expired.png'), fullPage: true });
    await page.goto('/zones');
    const row = main(page).getByRole('row').filter({ has: page.getByRole('link', { name: apex, exact: true }) });
    await expect(row).toContainText('Expired');
    await expect(row).toContainText('last observed');
  } finally {
    await page.goto('/zones');
    await api.del(`/zones/${name}`);
    await primaryApi.dispose();
    if (!stopped) await primary.stop();
  }
});
