// Unblock requests end to end: a user asks for a blocked name on the
// block page listener (a plain form POST with the name as Host), the
// request shows up on /requests, the admin approves it, and dig shows the
// name permitted; denying it afterwards blocks the name again.
import type { Page } from '@playwright/test';
import { env, expect, settle, test } from './fixtures';
import { suffix } from './support/api';
import { BLOCKLIST } from './support/kudurru';

function main(page: Page) {
  return page.getByRole('main');
}

async function dig(page: Page, name: string) {
  await page.goto('/tools');
  await settle(page);
  const panel = page.getByRole('region', { name: 'Query (dig)' });
  await panel.getByRole('textbox', { name: 'Name' }).fill(name);
  await panel.getByRole('button', { name: 'Query' }).click();
  const result = panel.locator('[aria-live="polite"]').last();
  await expect(result).toBeVisible();
  return result;
}

test('a request from the block page is approved on /requests and the name resolves', async ({ page, request }) => {
  const domain = `req-${suffix()}.${BLOCKLIST.blocked}`;
  const blockPage = env('KUDURRU_E2E_BLOCKPAGE');

  // The block page for the name offers the form.
  const shown = await request.get(`${blockPage}/some/path`, { headers: { Host: domain } });
  expect(shown.status()).toBe(200);
  expect(await shown.text()).toContain('<form method="post" action="/.kudurru/request">');

  // The user submits it: POST, then a redirect to the page with the outcome.
  const posted = await request.post(`${blockPage}/.kudurru/request`, {
    headers: { Host: domain, Origin: `http://${domain}` },
    form: { name: 'E2E user', reason: 'needed for <b>work</b>' },
    maxRedirects: 0,
  });
  expect(posted.status(), await posted.text()).toBe(303);
  expect(posted.headers()['location']).toBe('/?kudurru-request=sent');
  const pending = await request.get(`${blockPage}/?kudurru-request=sent`, { headers: { Host: domain } });
  expect(await pending.text()).toContain('An administrator has not decided yet');

  // The admin sees it on /requests and approves it for everyone.
  await page.goto('/requests');
  await settle(page);
  const row = main(page).getByTestId(`request-${domain}`);
  await expect(row).toBeVisible();
  await expect(row).toContainText('needed for <b>work</b>');
  await expect(row).toContainText('E2E user');
  await row.getByRole('button', { name: 'Approve' }).click();
  const dialog = page.getByRole('dialog', { name: `Allow ${domain}?` });
  await expect(dialog).toBeVisible();
  await dialog.getByRole('combobox', { name: 'Scope' }).selectOption('everyone');
  await dialog.getByRole('combobox', { name: 'Match' }).selectOption('exact');
  await dialog.getByRole('combobox', { name: 'Duration' }).selectOption('1h');
  await dialog.getByRole('textbox', { name: /^Note/ }).fill('for the e2e run');
  await dialog.getByRole('button', { name: 'Approve' }).click();
  await expect(page.getByText(`Allowed ${domain} for everyone`).first()).toBeVisible();

  // The request page shows the decision.
  await page.goto('/requests?state=Approved');
  await settle(page);
  await main(page).getByTestId(`request-${domain}`).getByRole('link', { name: domain }).click();
  await expect(main(page).getByRole('heading', { level: 1, name: domain })).toBeVisible();
  await expect(main(page).getByTestId('request-decision')).toContainText('for the e2e run');

  // dig: permitted once the lists are recompiled.
  await expect(async () => {
    const result = await dig(page, domain);
    await expect(result).toContainText('NOERROR');
    await expect(result).not.toContainText(/blocked/i);
  }).toPass({ timeout: 20_000 });

  // The block page now tells the user to try again.
  const approved = await request.get(`${blockPage}/`, { headers: { Host: domain } });
  expect(await approved.text()).toContain('An administrator allowed this name until');

  // Deny revokes it: blocked again.
  await page.goto('/requests?state=Approved');
  await settle(page);
  await main(page).getByTestId(`request-${domain}`).getByRole('button', { name: 'Deny' }).click();
  const deny = page.getByRole('dialog', { name: `Revoke the approval of ${domain}?` });
  await deny.getByRole('textbox', { name: /^Note/ }).fill('done');
  await deny.getByRole('button', { name: 'Deny' }).click();
  await expect(page.getByText(`Revoked the approval of ${domain}`).first()).toBeVisible();
  await expect(async () => {
    const result = await dig(page, domain);
    await expect(result).toContainText(/blocked/i);
  }).toPass({ timeout: 20_000 });
});
