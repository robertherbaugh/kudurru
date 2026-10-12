// Chapter: DNS (Zones, Upstreams).
import { choose, dialog, fresh, getPage, go, group, scroll, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('zones', async () => {
    await fresh();
    const page = getPage();
    await go('/zones');
    await shot('zones');
    await main().getByRole('button', { name: 'New zone' }).click();
    await settle(700);
    await shot('zone-wizard-type');
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(700);
    await dialog().getByRole('textbox', { name: /^Zone name/ }).fill('lab.example');
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(800);
    await shot('zone-wizard-records');
    await page.keyboard.press('Escape');
    await go('/zones');
    await main().getByRole('link', { name: 'example.internal.' }).click();
    await settle(1200);
    await shot('zone-detail');
    await main().getByRole('button', { name: 'Add record' }).click();
    await settle(700);
    await shot('record-editor');
  });
  await group('upstreams', async () => {
    await fresh();
    const page = getPage();
    await go('/upstreams');
    await shot('upstreams');
    await scroll(700);
    await go('/upstreams/default');
    await shot('upstream-group');
    await go('/upstreams');
    await main().getByRole('button', { name: 'New group' }).click();
    await settle(700);
    await shot('upstream-wizard-provider');
    await dialog().getByRole('radio', { name: /^Quad9/ }).check();
    await dialog().getByRole('button', { name: 'Next' }).click();
    await settle(800);
    await shot('upstream-wizard-servers');
  });
}
