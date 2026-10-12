// Chapter: unblock requests, and the block page the users see.
import { chromium } from 'playwright-core';
import { BLOCKPAGE, CHROME, GROUPS, dialog, fresh, getPage, go, group, settle, shot, main } from './lib.mjs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const OUT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../docs/guide/images');

export default async function () {
  await group('requests', async () => {
    await fresh();
    const page = getPage();
    await go('/requests');
    // The mock holds one request with markup in it (a test of escaping): deny it, so the shot shows real names.
    const odd = main().getByRole('row').filter({ hasText: 'graph.facebook.com' });
    if (await odd.count()) {
      await odd.getByRole('button', { name: 'Deny' }).click();
      await settle(500);
      await dialog().getByRole('button', { name: 'Deny' }).click();
      await settle(800);
    }
    await getPage().waitForTimeout(6000); // the toast of the denial goes away
    await shot('requests');
    await main().getByRole('row').filter({ hasText: 'tiktokv.com' }).getByRole('button', { name: 'Approve' }).click();
    await settle(800);
    await shot('request-approve');
  });

  // The block page of a real node (KUDURRU_GUIDE_BLOCKPAGE, see the header of guide-screenshots.mjs).
  if (!BLOCKPAGE) {
    console.log('# block page: skipped (KUDURRU_GUIDE_BLOCKPAGE is not set)');
    return;
  }
  if (GROUPS && !GROUPS.includes('block page')) return;
  console.log('# block page');
  const port = new URL(BLOCKPAGE).port;
  const browser = await chromium.launch({ executablePath: CHROME, args: ['--no-sandbox', '--host-resolver-rules=MAP ads.example.net 127.0.0.1'] });
  try {
    const ctx = await browser.newContext({ viewport: { width: 760, height: 1000 } });
    const p = await ctx.newPage();
    await p.goto(`http://ads.example.net:${port}/`);
    await p.screenshot({ path: path.join(OUT, 'block-page.png') });
    console.log('shot block-page');
    await p.getByLabel('Your name (optional)').fill('Mia');
    await p.getByLabel('Why do you need this site?').fill('I need it for the school video project.');
    await p.getByRole('button', { name: 'Request access' }).click();
    await p.waitForLoadState();
    await p.screenshot({ path: path.join(OUT, 'block-page-sent.png') });
    console.log('shot block-page-sent');
  } finally {
    await browser.close();
  }
}
