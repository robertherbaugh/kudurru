// Chapter: who is on the network (Groups, Clients, Policies).
import { choose, dialog, fresh, getPage, go, group, scroll, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('groups', async () => {
    await fresh();
    const page = getPage();
    await go('/groups');
    await shot('groups');
    await main().getByRole('button', { name: 'New group' }).click();
    await settle(700);
    await dialog().getByRole('textbox', { name: /^Name/ }).fill('guests');
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(700);
    await shot('group-wizard-members');
    await page.keyboard.press('Escape');
    await go('/groups/default/kids');
    await shot('group-detail');
  });
  await group('clients', async () => {
    await fresh();
    await go('/clients');
    await shot('clients');
    await main().getByRole('link', { name: 'robert-laptop' }).first().click();
    await settle(1200);
    await shot('client-detail');
  });
  await group('policies', async () => {
    await fresh();
    const page = getPage();
    await go('/policies');
    await shot('policies');
    await main().getByRole('button', { name: 'New policy' }).click();
    await settle(700);
    await shot('policy-wizard-who');
    await dialog().getByRole('textbox', { name: /^Name/ }).fill('guest-filter');
    await dialog().getByRole('checkbox', { name: /^kids/ }).check();
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(900);
    await shot('policy-wizard-what');
    for (const step of ['safe-search', 'how', 'when']) {
      await dialog().getByRole('button', { name: 'Next' }).click();
      await settle(800);
      await shot(`policy-wizard-${step}`);
    }
    await page.keyboard.press('Escape');
    await go('/policies/default/kids-bedtime');
    await shot('policy-detail');
    await scroll(750);
    await shot('policy-detail-services');
  });
}
