// Chapter: first sign-in.
import { fresh, getPage, go, group, settle, shot, main } from './lib.mjs';

export default async function () {
  await group('sign in', async () => {
    await fresh(null);
    const page = getPage();
    await shot('signin');
    await page.getByLabel('Username').fill('admin');
    await page.getByLabel('Password').fill('admin');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await settle(2500);
    await shot('first-dashboard');
    await page.getByRole('button', { name: 'Change password' }).first().click();
    await settle(600);
    await shot('change-password');
  });
}
