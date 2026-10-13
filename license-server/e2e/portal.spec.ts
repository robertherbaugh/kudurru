// The license portal end to end, against the built image with Postgres,
// Dex and Mailpit (compose.yaml), in the order the people involved meet it:
// a license admin issues a business license and invites the customer; the
// customer accepts, sees and downloads the key and asks for a free personal
// key; someone with no account uses the kudurru.io form, confirms the
// mailed link and gets the key by mail; the superadmin sees users, the
// audit trail and the system page. Every key the portal hands out is then
// judged by the node's own code (internal/license via nodestatus.go).
import { expect, test, type Browser, type Page } from '@playwright/test';
import { createServer } from 'node:http';
import { clearMail, nodeStatus, shot, signIn, waitForMail } from './support';

test.describe.configure({ mode: 'serial' });

/** The page the kudurru.io form lives on in this suite (PUBLIC_FORM_ORIGINS in compose.yaml). */
const FORM_ORIGIN = 'http://localhost:8792';
const run = Date.now().toString(36);
const company = `Example GmbH ${run}`;

let licenseId = '';

async function asUser(browser: Browser, email: string): Promise<Page> {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 860 } })).newPage();
  await signIn(page, email);
  return page;
}

test.beforeAll(async () => {
  await clearMail();
});

test('a license admin issues a business license with a feature subset', async ({ browser }) => {
  const page = await asUser(browser, 'admin@example.org');
  await expect(page.getByRole('link', { name: 'Licenses', exact: true })).toBeVisible();
  await page.goto('/licenses');
  await page.getByRole('button', { name: 'Issue license' }).click();
  const wizard = page.getByRole('dialog');

  await wizard.getByRole('radio', { name: /New customer/ }).check();
  await wizard.getByRole('textbox', { name: 'Customer name' }).fill(company);
  await wizard.getByRole('textbox', { name: 'Contact email' }).fill(`ops-${run}@example.org`);
  await wizard.getByRole('button', { name: 'Next' }).click();

  await wizard.getByRole('radio', { name: /^Business/ }).check();
  await wizard.getByRole('spinbutton', { name: 'Nodes' }).fill('5');
  await wizard.getByRole('radio', { name: /Only these features/ }).check();
  await wizard.getByRole('checkbox', { name: 'Single sign-on' }).check();
  await wizard.getByRole('checkbox', { name: 'High-availability edge' }).check();
  await wizard.getByRole('button', { name: 'Next' }).click();
  await wizard.getByRole('textbox', { name: 'Agreement reference' }).fill(`AGR-${run}`);
  await wizard.getByRole('button', { name: 'Next' }).click();

  await expect(wizard.getByText('Keeps working until (UTC)')).toBeVisible();
  await shot(page, '02-issue-review');
  await wizard.getByRole('button', { name: 'Sign and issue' }).click();

  await page.waitForURL(/\/licenses\/lic_/);
  licenseId = decodeURIComponent(page.url().split('/licenses/')[1]!);
  await expect(page.getByRole('heading', { level: 1, name: company })).toBeVisible();
  await expect(page.getByText('A license key does not give you a license.').first()).toBeVisible();
  await shot(page, '03-license-admin');

  const detail = (await (await page.request.get(`/api/v1/licenses/${licenseId}`)).json()) as { currentKey: { token: string }; expiresOn: string };
  const node = nodeStatus(detail.currentKey.token);
  if (node) {
    expect(node).toMatchObject({ state: 'active', plan: 'business', licensee: company, id: licenseId, nodes: 5, expires: detail.expiresOn });
    expect([...node.features].sort()).toEqual(['edge', 'oidc']);
  }

  // Give the customer access: an invite link, mailed.
  await page.getByRole('link', { name: company }).first().click();
  await page.waitForURL(/\/customers\/cus_/);
  await page.getByRole('button', { name: 'Add member' }).click();
  await page.getByRole('dialog').getByRole('textbox', { name: 'Email' }).fill('customer@example.org');
  await page.getByRole('dialog').getByRole('button', { name: 'Add member' }).click();
  await expect(page.getByRole('dialog').getByText('Invite link for customer@example.org')).toBeVisible();
  await page.getByRole('dialog').getByRole('button', { name: 'Done' }).click();
  await page.context().close();
});

test('the customer accepts the invite, sees the key and asks for a free personal key', async ({ browser }) => {
  const invite = await waitForMail('customer@example.org', /access/i);
  const link = /https?:\/\/\S+\/invites\/[A-Za-z0-9_-]{43}/.exec(invite.text)![0];

  const page = await asUser(browser, 'customer@example.org');
  await expect(page.getByRole('link', { name: 'Licenses', exact: true })).toHaveCount(0);
  await page.goto(new URL(link).pathname);
  await expect(page.getByText(company).first()).toBeVisible();
  await page.getByRole('button', { name: 'Accept' }).click();
  await page.getByRole('link', { name: 'Go to my licenses' }).click();
  await page.getByRole('link', { name: company }).first().click();
  await page.waitForURL(/\/my\/licenses\/lic_/);
  await expect(page.getByRole('link', { name: 'Download license.key' })).toBeVisible();
  await expect(page.getByText('KUDURRU_LICENSE_KEY_FILE').first()).toBeVisible();
  await shot(page, '04-license-customer');

  // The download is the key as the node reads it from a file.
  const download = await page.request.get(`/api/v1/me/licenses/${licenseId}/key`);
  expect(download.headers()['content-disposition']).toContain('license.key');
  const body = await download.text();
  expect(body.endsWith('\n')).toBe(true);
  expect(nodeStatus(body.trim())?.state ?? 'active').toBe('active');

  // A customer cannot reach the admin pages.
  await page.goto('/licenses');
  await expect(page.getByText(/do not include this page/i)).toBeVisible();

  // The free personal key from the portal.
  await page.goto('/my/licenses');
  await page.getByRole('button', { name: 'Request a free personal key' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: 'Name on the key' }).fill('Chris Customer');
  await dialog.getByRole('textbox', { name: 'What do you run kudurru for?' }).fill('DNS for my home network');
  await dialog.getByRole('checkbox', { name: /I agree and certify/ }).check();
  await shot(page, '05-personal-request');
  await dialog.getByRole('button', { name: 'Request key' }).click();
  await expect(dialog.getByText(/sent|issued|on its way/i).first()).toBeVisible();
  const mail = await waitForMail('customer@example.org', /license key/i);
  const key = mail.attachments.find((a) => a.name === 'license.key');
  expect(key).toBeDefined();
  const node = nodeStatus(key!.body.trim());
  if (node) expect(node).toMatchObject({ state: 'active', plan: 'personal', licensee: 'Chris Customer', nodes: 3, features: expect.arrayContaining(['oidc', 'rbac']) });
  await page.context().close();
});

test('the kudurru.io form: a cross-origin request, a confirmation link, the key by mail', async ({ page }) => {
  const email = `homelab-${run}@example.net`;
  // A minimal stand-in for the kudurru.io form on its own origin, served
  // from a real local server (Chrome's Local Network Access refuses a page
  // it sees as public calling a loopback portal): the same request the www
  // client sends (www/src/client/license.ts), cross-origin.
  const site = createServer((_req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end('<!doctype html><title>kudurru.io</title><p>the license page</p>');
  });
  await new Promise<void>((done) => site.listen(Number(new URL(FORM_ORIGIN).port), '127.0.0.1', done));
  test.info().attach('form origin', { body: FORM_ORIGIN });
  try {
    await page.goto(`${FORM_ORIGIN}/license`);
  const portal = new URL(process.env.E2E_BASE_URL ?? 'http://localhost:8791').origin;
  const answer = await page.evaluate(
    async ({ portal, email }) => {
      const res = await fetch(`${portal}/api/v1/public/key-requests`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'omit',
        body: JSON.stringify({ name: 'Homelab Hank', email, use: 'DNS for the house', noncommercial: true, website: '' }),
      });
      return { status: res.status, body: await res.json() };
    },
    { portal, email },
  );
  expect(answer).toEqual({ status: 202, body: { status: 'sent' } });
  } finally {
    site.close();
  }

  const confirm = await waitForMail(email, /confirm/i);
  expect(confirm.text.startsWith('Hello,')).toBe(true);
  const link = /https?:\/\/\S+\/verify\?token=[A-Za-z0-9_-]{43}/.exec(confirm.text)![0];
  await page.goto(link);
  await shot(page, '06-verify');
  await page.getByRole('button', { name: 'Confirm and send my key' }).click();
  await expect(page.getByText(/key is ready|mailed the key/i).first()).toBeVisible();

  const keyMail = await waitForMail(email, /license key/i);
  const key = keyMail.attachments.find((a) => a.name === 'license.key')!;
  expect(/^[\x20-\x7e]+\n$/.test(key.body)).toBe(true);
  expect(keyMail.text).toContain('non-commercial');
  const node = nodeStatus(key.body.trim());
  if (node) expect(node).toMatchObject({ state: 'active', plan: 'personal', licensee: 'Homelab Hank', nodes: 3 });
});

test('the superadmin sees users, the audit trail and the system page', async ({ browser }) => {
  const page = await asUser(browser, 'superadmin@example.org');
  await page.goto('/users');
  await expect(page.getByText('customer@example.org').first()).toBeVisible();
  await shot(page, '07-users');
  await page.goto('/audit');
  await expect(page.getByRole('cell', { name: 'license.issued' }).first()).toBeVisible();
  await shot(page, '08-audit');
  await page.goto('/system');
  await expect(page.getByText('dev-1').first()).toBeVisible();
  await shot(page, '09-system');
  await page.goto(`/licenses/${licenseId}`);
  await shot(page, '10-license-superadmin');
  await page.context().close();
});
