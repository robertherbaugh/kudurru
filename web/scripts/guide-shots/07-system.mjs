// Chapter: the System section (Nodes, Settings, Tools, Diagnosis).
import { choose, dialog, fresh, getPage, go, group, scroll, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('system', async () => {
    await fresh();
    const page = getPage();
    await go('/nodes');
    await shot('nodes');
    await go('/settings');
    await shot('settings');
    await main().getByRole('radio', { name: 'Expert' }).check().catch(async () => main().getByText('Expert').click());
    await settle(800);
    await shot('settings-expert');
    await go('/tools');
    const dig = page.getByRole('region', { name: 'Query (dig)' });
    await dig.getByRole('textbox', { name: 'Name' }).fill('www.google.com');
    await dig.getByRole('button', { name: 'Query' }).click();
    await settle(1200);
    await shot('tools-query');
    await page.getByRole('button', { name: 'Run checks' }).click();
    await settle(1200);
    await scroll(400);
    await shot('tools-doctor');
    await go('/messages');
    await shot('diagnosis');
  });
  await group('access', async () => {
    await fresh();
    const page = getPage();
    await go('/access');
    await shot('access');
    await page.getByRole('textbox', { name: 'Description' }).fill('home-assistant');
    await page.getByRole('button', { name: 'Create token' }).click();
    await settle(1000);
    await shot('access-token-created');
    await page.getByText('Identity', { exact: true }).first().click();
    await settle(1000);
    await shot('access-identity');
    await page.getByText('Audit', { exact: true }).first().click();
    await settle(1000);
    await shot('access-audit');
  });
  await group('appearance', async () => {
    await fresh();
    const page = getPage();
    await page.getByRole('button', { name: 'Appearance' }).click();
    await settle(600);
    await shot('appearance');
  });
}
