import { expect, expectRendered, main, settle, test } from './fixtures';
import { adminApi, suffix } from './support/api';
import type { DNSZone } from '../src/api/types';

test.use({
  trace: 'off', screenshot: 'off', video: 'off',
  // The existing complete publisher refuses record reads while new desired
  // policy replaces the primary receipt. Require recovery below; never retry PUT.
  allowConsole: /status of 503.*\/api\/v1\/zones\/notify-options-[a-z0-9]+\/records\)/,
});

test('NOTIFY key/source persist and clear through the real zone editor', async ({ page }) => {
  const name = `notify-options-${suffix()}`;
  await page.goto('/');
  const api = await adminApi(page);
  await api.put(`/objects/dnszones/${name}`, {
    apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone',
    metadata: { name, annotations: { 'test.example/preserve': 'original' } },
    spec: { zoneName: `${name}.example.`, type: 'primary', policy: { notify: 'none', transferTSIGKeyRefs: ['transfer.example.'] } },
  });
  const published = async () => {
    const { body: zone } = await api.get<DNSZone>(`/objects/dnszones/${name}`);
    return zone.status?.observedGeneration === zone.metadata.generation && !!zone.status?.contentHash &&
      !!zone.status?.serial && zone.status?.conditions?.some((condition) => condition.type === 'Ready' && condition.status === 'True');
  };
  try {
    // Observe controller completion before capturing the editor's original ETag.
    // A concurrent later change still refuses the one PUT; never replay it.
    await expect.poll(published).toBe(true);
    await expect.poll(async () => (await api.get(`/zones/${name}/records`)).status).toBe(200);
    await page.goto(`/zones/${name}`);
    await settle(page);
    const section = main(page).getByTestId('section-zone-settings');
    await section.getByRole('button', { name: 'Edit Zone settings' }).click();
    await section.getByRole('textbox', { name: 'NOTIFY TSIG key' }).fill('notify.example.');
    await section.getByRole('textbox', { name: 'NOTIFY source address' }).fill('::1');
    const save = page.waitForResponse((response) => response.request().method() === 'PUT' && new URL(response.url()).pathname === `/api/v1/objects/dnszones/${name}`);
    await section.getByRole('button', { name: 'Save Zone settings' }).click();
    expect((await save).status(), 'single original edit').toBe(200);
    await expect(section.getByRole('button', { name: 'Edit Zone settings' })).toBeVisible();
    await expect.poll(published).toBe(true);
    await page.reload();
    await settle(page);
    await expect(section).toContainText('notify.example.');
    await expect(section).toContainText('::1');
    const saved = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    expect(saved.spec.policy).toMatchObject({ notify: 'none', notifyTSIGKeyRef: 'notify.example.', notifySource: '::1', transferTSIGKeyRefs: ['transfer.example.'] });
    expect(saved.metadata.annotations?.['test.example/preserve']).toBe('original');

    await section.getByRole('button', { name: 'Edit Zone settings' }).click();
    await section.getByRole('textbox', { name: 'NOTIFY TSIG key' }).fill('');
    await section.getByRole('textbox', { name: 'NOTIFY source address' }).fill('');
    const clear = page.waitForResponse((response) => response.request().method() === 'PUT' && new URL(response.url()).pathname === `/api/v1/objects/dnszones/${name}`);
    await section.getByRole('button', { name: 'Save Zone settings' }).click();
    expect((await clear).status(), 'single original clear').toBe(200);
    await expect(section.getByRole('button', { name: 'Edit Zone settings' })).toBeVisible();
    const cleared = (await api.get<DNSZone>(`/objects/dnszones/${name}`)).body;
    expect(cleared.spec.policy?.notifyTSIGKeyRef).toBeUndefined();
    expect(cleared.spec.policy?.notifySource).toBeUndefined();
    expect(cleared.spec.policy?.transferTSIGKeyRefs).toEqual(['transfer.example.']);
    await expect.poll(published).toBe(true);
    await expect.poll(async () => (await api.get(`/zones/${name}/records`)).status).toBe(200);
    await page.reload();
    await settle(page);
    await expectRendered(page);
    await expect(section).toContainText('First transfer key (transfer.example.)');
  } finally { await api.del(`/zones/${name}`); }
});
