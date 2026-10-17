// Chapter: what a viewer sees.
import { fresh, go, group, shot } from './lib.mjs';

export default async function () {
  await group('viewer', async () => {
    await fresh('viewer');
    await go('/requests');
    await shot('viewer-not-permitted');
    await go('/lists');
  });
}
