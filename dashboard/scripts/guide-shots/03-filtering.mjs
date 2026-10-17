// Chapter: filtering (Lists, Domains, Allowlist).
import { dialog, fresh, getPage, go, group, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('lists', async () => {
    await fresh();
    const page = getPage();
    await go('/lists');
    await shot('lists');
    await main().getByRole('button', { name: 'New list' }).click();
    await settle(700);
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(700);
    await shot('list-wizard-sources');
    await dialog().getByRole('checkbox', { name: /HaGeZi Multi LIGHT/ }).check();
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(800);
    await shot('list-wizard-options');
    await page.keyboard.press('Escape');
    await go('/lists/default/family');
    await shot('list-detail');
    await main().getByRole('button', { name: 'Entries' }).first().click();
    await settle(900);
    await shot('list-source-entries');
  });
  await group('domains', async () => {
    await fresh();
    const page = getPage();
    await go('/domains');
    await main().getByRole('button', { name: 'Add domains' }).click();
    await settle(700);
    await shot('domains-add');
    await page.keyboard.press('Escape');
    await go('/allowlist');
    await shot('allowlist');
  });
}
