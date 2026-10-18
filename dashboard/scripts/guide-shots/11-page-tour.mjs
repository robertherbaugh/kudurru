// Pages that complete the dashboard guide, using the same mock as the app.
import { fresh, getPage, go, group, shot } from './lib.mjs';

export default async function () {
  await group('page-tour', async () => {
    await fresh();
    for (const [route, name] of [['/domains', 'domains'], ['/denylist', 'denylist'], ['/license', 'license']]) {
      await go(route);
      await shot(name);
    }
    await go('/tools');
    await getPage().getByRole('button', { name: 'Import from DNS server', exact: true }).click();
    await shot('import-connect');
  });
}
