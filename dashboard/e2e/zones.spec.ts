// Zone settings and zone delete on the zone's page against the real API:
// a forward zone's upstream group is edited and survives a reload, and a
// primary zone with a record is deleted with its record set, after which
// its names no longer answer from the zone (POST /dns/query, Tools → Query).
import { expect, expectRendered, main, settle, test } from './fixtures';
import { choose } from './support/astryx';
import { adminApi } from './support/api';
import type { DNSZone } from '../src/api/types';

const zone = (name: string, spec: unknown) => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec });

test('a forward zone’s settings are edited on its page', async ({ page }) => {
  const name = `fwd-${Date.now().toString(36)}-example`;
  const apex = `${name.replace(/-example$/, '')}.example.`;
  await page.goto('/');
  const api = await adminApi(page);
  await api.put(`/objects/dnszones/${name}`, zone(name, { zoneName: apex, type: 'forward', forward: {} }));
  try {
    await page.goto(`/zones/${name}`);
    await settle(page);
    const section = main(page).getByTestId('section-zone-settings');
    await expect(section).toContainText('default');
    await section.getByRole('button', { name: 'Edit Zone settings' }).click();
    await choose(section.getByRole('combobox', { name: /DNSSEC validation/ }), 'Validate');
    await section.getByRole('button', { name: 'Save Zone settings' }).click();
    await expect(section.getByRole('button', { name: 'Save Zone settings' })).toHaveCount(0);
    await page.reload();
    await settle(page);
    await expect(main(page).getByTestId('section-zone-settings')).toContainText('validate');
    const got = await api.get<{ spec: { forward?: { dnssecValidation?: boolean } } }>(`/objects/dnszones/${name}`);
    expect(got.body.spec.forward?.dnssecValidation).toBe(true);
  } finally {
    await api.del(`/zones/${name}`);
  }
});

test('a primary zone is deleted with its records from its page', async ({ page, pendingZoneReadPaths }) => {
  const label = `del-${Date.now().toString(36)}`;
  const name = `${label}-example`;
  const apex = `${label}.example.`;
  const host = `www.${label}.example`;
  await page.goto('/');
  const api = await adminApi(page);
  await api.put(`/objects/dnszones/${name}`, zone(name, { zoneName: apex, type: 'primary' }));
  // Creation acknowledges desired storage before the primary's Ready status
  // and complete serving publication. Read that accepted status before the
  // dependent record command; never replay a refused mutation.
  await expect.poll(async () => {
    const current = await api.get<DNSZone>(`/objects/dnszones/${name}`);
    if (current.status !== 200) return false;
    const { metadata, status } = current.body;
    return status?.observedGeneration === metadata.generation && status?.conditions?.some((c) => c.type === 'Ready' && c.status === 'True' && c.observedGeneration === metadata.generation) === true;
  }).toBe(true);
  await api.post(`/zones/${name}/records`, { dnsName: host, recordType: 'A', targets: ['192.0.2.77'] });
  await expect
    .poll(async () => (await api.post<{ status: string }>('/dns/query', { name: host, type: 'A' })).status, { timeout: 30_000 })
    .toBe('authoritative');

  const recordsTarget = `/api/v1/zones/${name}/records`;
  pendingZoneReadPaths.add(recordsTarget);
  let refusedRead = false;
  await page.route(new URL(recordsTarget, page.url()).href, async (route) => {
    if (route.request().method() !== 'GET' || refusedRead) return route.continue();
    refusedRead = true;
    // The committed zone can briefly precede its complete serving publication.
    // Recover by reading the real records before reviewing the original delete.
    await route.fulfill({ status: 503, contentType: 'application/json', json: {
      error: { key: 'unavailable', message: 'committed zone read: serving: generation pending' }, took_ms: 0,
    } });
  });
  let deleteRequests = 0;
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (request.method() === 'DELETE' && url.pathname === `/api/v1/zones/${name}` && !url.search) deleteRequests++;
  });
  await page.goto(`/zones/${name}`);
  await settle(page);
  await expectRendered(page);
  await expect(main(page).getByRole('table', { name: `Records of ${apex}`, exact: true })).toContainText('192.0.2.77');
  await main(page).getByRole('button', { name: 'Delete zone' }).click();
  const dialog = page.getByRole('dialog', { name: `Delete zone ${apex}` });
  await expect(dialog).toContainText('1 records in 1 record sets are deleted');
  const confirm = dialog.getByRole('button', { name: 'Delete zone' });
  await expect(confirm).toBeDisabled();
  await dialog.getByRole('textbox', { name: /Type the zone name/ }).fill(apex.replace(/\.$/, ''));
  await confirm.click();
  await expect(page).toHaveURL((u) => u.pathname === '/zones');
  await settle(page);
  await expect(main(page).getByRole('link', { name: apex, exact: true })).toHaveCount(0);

  expect((await api.get(`/objects/dnszones/${name}`)).status).toBe(404);
  await expect
    .poll(async () => (await api.post<{ status: string }>('/dns/query', { name: host, type: 'A' })).status, { timeout: 30_000 })
    .not.toBe('authoritative');
  expect(refusedRead, 'exercise the committed-zone read refusal before recovery').toBe(true);
  expect(deleteRequests, 'the reviewed zone deletion must be submitted exactly once').toBe(1);
});

test('the root zone cannot be deleted', async ({ page }) => {
  await page.goto('/zones');
  await settle(page);
  await expect(main(page).getByRole('button', { name: 'Delete zone .' })).toBeDisabled();
});
