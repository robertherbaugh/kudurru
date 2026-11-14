// The inventory view and client-group record views against a real node.
//
// Inventory (D397): a node whose credentials.allowedFiles grants one bearer
// token file, and a kudurru webhook inventory peer on 127.0.0.1
// (support/inventorypeer.ts). A webhook Integration with the "clients"
// capability makes the node poll the peer; its devices appear in
// Filtering > Inventory although none of them ever sent a DNS query, with
// their MACs, tags and the address state in words, linked to the
// Integration that reported them, and the filters reach the server.
//
// Record views (D403): on the run's node, a record served to one ClientGroup
// is added through the record editor next to the default data of the same
// name and type, listed with its view, and deleted alone. A view of a group
// in another namespace is edited in the record set that holds it and
// deleted without touching the default namespace's view of the same name.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { randomBytes } from 'node:crypto';
import { env, expect, expectRendered, main, selector, settle, SIGNED_OUT_401, test } from './fixtures';
import { choose } from './support/astryx';
import { adminApi, object, suffix } from './support/api';
import { startInventoryPeer, type InventoryPeer } from './support/inventorypeer';
import { ADMIN_USER, startServer, type Server } from './support/kudurru';
import type { DNSZone, ZoneRecord } from '../src/api/types';

test.describe('inventory with a webhook inventory peer', () => {
  let server: Server;
  let peer: InventoryPeer;
  let dir: string;
  let tokenFile: string;
  const password = `inventory-${Date.now()}-password`;
  const token = randomBytes(24).toString('hex');
  test.use({ storageState: { cookies: [], origins: [] }, allowConsole: SIGNED_OUT_401 });

  test.beforeAll(async () => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'kudurru-e2e-inventory-'));
    tokenFile = path.join(dir, 'webhook-token');
    fs.writeFileSync(tokenFile, token, { mode: 0o600 });
    peer = await startInventoryPeer(token, [
      { id: 'phone', name: 'e2e-phone', hostname: 'phone.example.test', ips: ['192.0.2.80'], macs: ['02:00:00:00:00:80'], tags: ['family'] },
      { id: 'printer', name: 'e2e-printer', hostname: 'printer.example.test', ips: ['192.0.2.81'], macs: ['02:00:00:00:00:81'], tags: [] },
    ]);
    server = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
      upstream: env('KUDURRU_E2E_UPSTREAM'),
      node: 'e2e-inventory',
      adminPassword: password,
      allowedCredentialFiles: [tokenFile],
    });
  });
  test.afterAll(async () => {
    await server?.stop();
    await peer?.close();
    if (dir) fs.rmSync(dir, { recursive: true, force: true });
  });

  test('lists devices that never queried, with their MACs, state and reporting Integration', async ({ page }) => {
    test.setTimeout(150_000);
    await page.goto(server.url);
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();
    await settle(page);

    const me = (await (await page.request.get(`${server.url}/api/v1/auth/me`)).json()) as { csrf: string };
    const created = await page.request.post(`${server.url}/api/v1/objects/integrations`, {
      headers: { 'X-Kudurru-CSRF': me.csrf },
      data: {
        apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'devices', namespace: 'default' },
        spec: { type: 'webhook', credentials: { bearerToken: { file: tokenFile } }, webhook: { endpoint: peer.url } },
      },
    });
    expect(created.ok(), `create Integration: ${created.status()} ${await created.text()}`).toBe(true);

    // The host polls the peer and publishes the observation; the inventory
    // lists it from the serving cut (no DNS query from these addresses).
    await expect
      .poll(async () => {
        const r = await page.request.get(`${server.url}/api/v1/inventory?kind=inventory`);
        if (!r.ok()) return 0;
        return ((await r.json()) as { items: unknown[] }).items.length;
      }, { timeout: 90_000, intervals: [2_000] })
      .toBe(2);
    expect(peer.polls()).toBeGreaterThan(0);

    await page.goto(`${server.url}/clients`);
    await settle(page);
    await main(page).getByRole('link', { name: 'Inventory' }).click();
    await expect(page).toHaveURL(/\/inventory$/);
    await settle(page);
    await expectRendered(page);
    const table = main(page).getByRole('table', { name: 'Inventory devices' });
    const phone = table.getByRole('row').filter({ hasText: 'e2e-phone' });
    await expect(phone).toContainText('02:00:00:00:00:80');
    await expect(phone).toContainText('192.0.2.80');
    await expect(phone).toContainText('Assigned');
    await expect(phone).toContainText('tags family');
    await expect(phone).toContainText('Live');
    await expect(phone.getByRole('link', { name: 'Integration devices' })).toHaveAttribute('href', '/integrations/default/devices');
    await expect(main(page).getByTestId('inventory-meta')).toContainText('Answered by node e2e-inventory');

    // Filters are server-side and kept in the URL.
    const requests: string[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/api/v1/inventory') requests.push(new URL(r.url()).search);
    });
    await main(page).getByRole('textbox', { name: /Find a device/ }).fill('00:81');
    await expect(table.getByRole('row').filter({ hasText: 'e2e-phone' })).toHaveCount(0);
    await expect(table.getByRole('row').filter({ hasText: 'e2e-printer' })).toHaveCount(1);
    expect(requests.some((s) => s.includes('q=00%3A81'))).toBe(true);
    await expect(page).toHaveURL(/q=00%3A81/);
    await choose(selector(main(page), 'Kind'), 'DHCP leases');
    await expect(main(page).getByText('No device matches these filters.')).toBeVisible();
    expect(requests.some((s) => s.includes('kind=dhcp'))).toBe(true);

    await page.goto(`${server.url}/inventory`);
    await settle(page);
    await main(page).getByRole('table', { name: 'Inventory devices' }).getByRole('link', { name: 'Integration devices' }).first().click();
    await expect(page).toHaveURL(/\/integrations\/default\/devices$/);
  });
});

test('a client-group view is added beside the default data, listed and deleted alone', async ({ page }) => {
  test.setTimeout(90_000);
  const label = `views-${suffix()}`;
  const name = `${label}-example`;
  const apex = `${label}.example.`;
  const host = `www.${label}.example`;
  const group = `${label}-lan`;
  await page.goto('/');
  const api = await adminApi(page);
  await api.put(`/objects/clientgroups/default/${group}`, object('ClientGroup', group, { clients: [{ ip: '192.0.2.200' }] }));
  await api.put(`/objects/dnszones/${name}`, { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { zoneName: apex, type: 'primary' } });
  try {
    await expect.poll(async () => {
      const current = await api.get<DNSZone>(`/objects/dnszones/${name}`);
      if (current.status !== 200) return false;
      const { metadata, status } = current.body;
      return status?.conditions?.some((c) => c.type === 'Ready' && c.status === 'True' && c.observedGeneration === metadata.generation) === true;
    }, { timeout: 30_000 }).toBe(true);
    await api.post(`/zones/${name}/records`, { dnsName: host, recordType: 'A', targets: ['203.0.113.10'] });

    await page.goto(`/zones/${name}`);
    await settle(page);
    const records = main(page).getByRole('table', { name: `Records of ${apex}`, exact: true });
    await expect(records).toContainText('203.0.113.10');
    // The SQLite runtime lists the served zone and says what that leaves out.
    await expect(main(page)).toContainText('This list shows what this node serves');
    await main(page).getByRole('button', { name: 'Add record' }).click();
    const dialog = page.getByRole('dialog');
    await choose(selector(dialog, 'Served to'), 'Published only (public)');
    await expect(dialog).toContainText('A public record is stored but not listed here');
    await dialog.getByRole('textbox', { name: /^Name/ }).first().fill('www');
    await dialog.getByRole('textbox', { name: /IPv4 address/ }).fill('10.0.0.10');
    await choose(selector(dialog, 'Served to'), `Client group ${group}`);
    await expect(dialog).toContainText('carry the default data only');
    await expect(dialog).toContainText('zone-client-view');
    await dialog.getByRole('button', { name: 'Add record' }).click();
    await expect(dialog).toHaveCount(0);

    // On the SQLite runtime the list is the served zone: wait for the
    // committed view to be published, rereading the page.
    const viewRow = records.getByRole('row').filter({ hasText: `view ${group}` });
    await expect.poll(async () => {
      if ((await viewRow.count()) > 0) return true;
      await page.reload();
      await settle(page);
      return (await viewRow.count()) > 0;
    }, { timeout: 30_000, intervals: [1_000] }).toBe(true);
    await expect(viewRow).toContainText('10.0.0.10');
    await expect(records.getByRole('row').filter({ hasText: '203.0.113.10' })).toHaveCount(1);
    const both = (await api.get<{ items: ZoneRecord[] }>(`/zones/${name}/records`)).body.items.filter((r) => r.recordType === 'A');
    expect(both.map((r) => `${r.kudurru?.view ?? ''}=${r.targets.join(',')}`).sort()).toEqual(['=203.0.113.10', `${group}=10.0.0.10`]);
    // A served view row names its ClientGroup's namespace and, proven by
    // the one desired endpoint behind it, its record set.
    expect(both.find((r) => r.kudurru?.view)).toMatchObject({ recordSet: `default/api-${name}`, namespace: 'default' });

    await viewRow.getByRole('button', { name: 'Delete' }).click();
    const confirm = page.getByRole('alertdialog');
    await expect(confirm).toContainText(`served to client group ${group} in record set default/api-${name}`);
    await confirm.getByRole('button', { name: 'Delete record' }).click();
    await expect.poll(async () => {
      await page.reload();
      await settle(page);
      return records.getByRole('row').filter({ hasText: `view ${group}` }).count();
    }, { timeout: 30_000, intervals: [1_000] }).toBe(0);
    await expect(records.getByRole('row').filter({ hasText: '203.0.113.10' })).toHaveCount(1);
    const left = (await api.get<{ items: ZoneRecord[] }>(`/zones/${name}/records`)).body.items.filter((r) => r.recordType === 'A');
    expect(left.map((r) => `${r.kudurru?.view ?? ''}=${r.targets.join(',')}`)).toEqual(['=203.0.113.10']);
  } finally {
    await api.del(`/zones/${name}`);
    await api.del(`/objects/clientgroups/default/${group}`);
  }
});

test('a view of a non-default namespace is edited in its own record set and deleted alone', async ({ page }) => {
  test.setTimeout(120_000);
  const label = `nsviews-${suffix()}`;
  const name = `${label}-example`;
  const apex = `${label}.example.`;
  const host = `nas.${label}.example`;
  const group = `${label}-lan`;
  const kidsSet = `${label}-kids`;
  await page.goto('/');
  const api = await adminApi(page);
  await api.put(`/objects/clientgroups/default/${group}`, object('ClientGroup', group, { clients: [{ ip: '192.0.2.201' }] }));
  await api.put(`/objects/clientgroups/kids/${group}`, { ...object('ClientGroup', group, { clients: [{ ip: '192.0.2.202' }] }), metadata: { name: group, namespace: 'kids' } });
  await api.put(`/objects/dnszones/${name}`, { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec: { zoneName: apex, type: 'primary' } });
  const kidsEndpoints = async () =>
    (await api.get<{ spec: { endpoints: { dnsName: string; targets: string[]; kudurru?: { view?: string; comment?: string } }[] } }>(`/objects/dnsrecordsets/kids/${kidsSet}`));
  try {
    await expect.poll(async () => {
      const current = await api.get<DNSZone>(`/objects/dnszones/${name}`);
      if (current.status !== 200) return false;
      const { metadata, status } = current.body;
      return status?.conditions?.some((c) => c.type === 'Ready' && c.status === 'True' && c.observedGeneration === metadata.generation) === true;
    }, { timeout: 30_000 }).toBe(true);
    // default/lan through the records API (default/api-<zone>), kids/lan in
    // a record set of the kids namespace that is not api-<zone>.
    await api.post(`/zones/${name}/records`, { dnsName: host, recordType: 'A', targets: ['10.0.0.20'], kudurru: { view: group } });
    await api.put(`/objects/dnsrecordsets/kids/${kidsSet}`, {
      apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: kidsSet, namespace: 'kids' },
      spec: { zoneRef: { name }, endpoints: [{ dnsName: host, recordType: 'A', targets: ['10.9.0.20'], recordTTL: 300, kudurru: { view: group, comment: 'kids NAS' } }] },
    });

    await page.goto(`/zones/${name}`);
    await settle(page);
    const records = main(page).getByRole('table', { name: `Records of ${apex}`, exact: true });
    const kidsRow = records.getByRole('row').filter({ hasText: `view kids/${group}` });
    await expect.poll(async () => {
      if ((await kidsRow.count()) > 0) return true;
      await page.reload();
      await settle(page);
      return (await kidsRow.count()) > 0;
    }, { timeout: 30_000, intervals: [1_000] }).toBe(true);
    await expect(kidsRow).toContainText('10.9.0.20');
    await expect(kidsRow).toContainText(`kids/${kidsSet}`);
    await expect(kidsRow.getByRole('link', { name: `view kids/${group}` })).toHaveAttribute('href', `/groups/kids/${group}`);
    const listed = (await api.get<{ items: ZoneRecord[]; source: string }>(`/zones/${name}/records`)).body;
    expect(listed.source).toBe('served');
    expect(listed.items.filter((r) => r.kudurru?.view).map((r) => `${r.namespace}/${r.kudurru?.view} ${r.recordSet}`).sort()).toEqual([`default/${group} default/api-${name}`, `kids/${group} kids/${kidsSet}`]);
    // The SOA names no record set, so it cannot be edited here.
    await expect(records.getByRole('row').filter({ hasText: 'SOA' }).getByRole('button', { name: 'Edit' })).toBeDisabled();

    await kidsRow.getByRole('button', { name: 'Edit' }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toContainText(`client group kids/${group}`);
    await expect(dialog.getByRole('textbox', { name: /^Record set/ })).toHaveValue(`kids/${kidsSet}`);
    await dialog.getByRole('textbox', { name: /IPv4 address/ }).fill('10.9.0.21');
    await dialog.getByRole('button', { name: 'Save record' }).click();
    await expect(dialog).toHaveCount(0);
    // The edit replaced the kids endpoint in place, kept its comment, and
    // wrote nothing into the default namespace.
    const edited = (await kidsEndpoints()).body.spec.endpoints;
    expect(edited.map((e) => `${e.kudurru?.view}=${e.targets.join(',')} ${e.kudurru?.comment ?? ''}`)).toEqual([`${group}=10.9.0.21 kids NAS`]);
    expect((await api.get(`/objects/dnsrecordsets/kids/api-${name}`)).status).toBe(404);
    await expect.poll(async () => {
      await page.reload();
      await settle(page);
      return (await kidsRow.textContent()) ?? '';
    }, { timeout: 30_000, intervals: [1_000] }).toContain('10.9.0.21');

    await kidsRow.getByRole('button', { name: 'Delete' }).click();
    const confirm = page.getByRole('alertdialog');
    await expect(confirm).toContainText(`served to client group kids/${group} in record set kids/${kidsSet}`);
    await confirm.getByRole('button', { name: 'Delete record' }).click();
    await expect.poll(async () => (await kidsEndpoints()).status, { timeout: 15_000 }).toBe(404);
    await expect.poll(async () => {
      await page.reload();
      await settle(page);
      return kidsRow.count();
    }, { timeout: 30_000, intervals: [1_000] }).toBe(0);
    const left = (await api.get<{ items: ZoneRecord[] }>(`/zones/${name}/records`)).body.items.filter((r) => r.kudurru?.view);
    expect(left.map((r) => `${r.namespace}/${r.kudurru?.view}=${r.targets.join(',')}`)).toEqual([`default/${group}=10.0.0.20`]);
  } finally {
    await api.del(`/zones/${name}`);
    await api.del(`/objects/clientgroups/default/${group}`);
    await api.del(`/objects/clientgroups/kids/${group}`);
  }
});
