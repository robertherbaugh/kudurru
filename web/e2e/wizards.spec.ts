// The creation wizards of spa-settings against the real API: a forward
// zone through /zones/new and an upstream group from a provider preset
// through /upstreams/new. Each test removes what it created.
import { expect, expectRendered, main, settle, test } from './fixtures';

test('the zone wizard creates a forward zone', async ({ page }) => {
  const apex = `fwd-${Date.now().toString(36)}.example`;
  await page.goto('/zones');
  await settle(page);
  await main(page).getByRole('button', { name: 'New zone' }).click();
  await expect(page).toHaveURL(/\/zones\/new$/);
  await settle(page);
  await main(page).getByRole('radio', { name: /^Forward/ }).check();
  await main(page).getByRole('button', { name: 'Next' }).click();
  await main(page).getByRole('textbox', { name: /^Zone name/ }).fill(apex);
  await main(page).getByRole('button', { name: 'Next' }).click();
  const group = main(page).getByRole('combobox', { name: /Upstream group/ });
  await expect(group.getByRole('option', { name: 'default', exact: true })).toBeAttached();
  await group.selectOption('default');
  await main(page).getByRole('button', { name: 'Next' }).click();
  await expect(main(page).getByTestId('zone-review')).toContainText(`${apex}.`);
  await main(page).getByRole('button', { name: 'Create zone' }).click();
  await expect(page).toHaveURL((u) => decodeURIComponent(u.pathname) === `/zones/${apex}`);
  await settle(page);
  await expectRendered(page);

  const res = await page.request.get(`/api/v1/objects/dnszones/${apex}`);
  expect(res.ok(), `GET dnszones/${apex}`).toBe(true);
  const zone = (await res.json()) as { spec: { type: string; forward?: { upstreamGroup?: string } } };
  expect(zone.spec.type).toBe('forward');
  expect(zone.spec.forward?.upstreamGroup).toBe('default');
  const csrf = await page.evaluate(async () => ((await (await fetch('/api/v1/auth/me')).json()) as { csrf?: string }).csrf ?? '');
  expect((await page.request.delete(`/api/v1/objects/dnszones/${apex}`, { headers: { 'X-Kudurru-CSRF': csrf } })).ok()).toBe(true);
});

test('the upstream wizard creates a group from a provider preset', async ({ page }) => {
  const name = `e2e-quad9-${Date.now().toString(36)}`;
  await page.goto('/upstreams');
  await settle(page);
  await main(page).getByRole('button', { name: 'New group' }).click();
  await expect(page).toHaveURL(/\/upstreams\/new$/);
  await settle(page);
  await main(page).getByRole('radio', { name: /^Quad9/ }).check();
  await main(page).getByRole('button', { name: 'Next' }).click();
  await main(page).getByRole('combobox', { name: 'Transport' }).selectOption('dot');
  await expect(main(page).getByTestId('preset-servers')).toContainText('tls://dns.quad9.net');
  await main(page).getByRole('button', { name: 'Next' }).click();
  await main(page).getByRole('textbox', { name: /^Name/ }).fill(name);
  await main(page).getByRole('combobox', { name: 'Strategy' }).selectOption('strict');
  await main(page).getByRole('button', { name: 'Next' }).click();
  await expect(main(page).getByTestId('upstream-review')).toContainText('quad9-dot=tls://dns.quad9.net');
  await main(page).getByRole('button', { name: 'Create group' }).click();
  await expect(page).toHaveURL(new RegExp(`/upstreams/${name}$`));
  await settle(page);
  await expect(main(page).getByRole('heading', { level: 1 })).toHaveText(name);
  await expect(main(page).getByRole('table', { name: `Servers of ${name}` })).toContainText('tls://dns.quad9.net');
  await expect(main(page).getByTestId('group-state')).toContainText('Strict order');
  await expectRendered(page);

  // Delete it again from its page.
  await main(page).getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page).toHaveURL(/\/upstreams$/);
  await expect(main(page).getByTestId(`group-${name}`)).toHaveCount(0);
});
