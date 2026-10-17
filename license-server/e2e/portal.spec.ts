// The license portal end to end, against the built image with Postgres,
// Dex and Mailpit (compose.yaml), in the order the people involved meet it:
// a license admin issues a business license and invites the customer; the
// customer accepts, sees and downloads the key and asks for a free personal
// key, which waits until a license admin approves it with the one-click
// link of the approval mail (ADMIN_NOTIFY_EMAIL); someone with no account
// uses the kudurru.io form, confirms the mailed link, waits for the same
// approval and gets the key by mail; another request is rejected from its
// link and the note reaches the requester; a business request from the
// form reaches the Requests page and both mailboxes, and a license admin
// issues a license from it; the superadmin sees users, the audit trail and
// the system page, and deletes a user. Every key the portal hands out is
// judged by the node's own code (internal/license via nodestatus.go).
import { expect, test, type Browser, type Page } from '@playwright/test';
import { createServer } from 'node:http';
import { clearMail, listMail, nodeStatus, PASSWORD, shot, signIn, waitForKeyMail, waitForMail } from './support';

test.describe.configure({ mode: 'serial' });

/** The page the kudurru.io form lives on in this suite (PUBLIC_FORM_ORIGINS in compose.yaml). */
const FORM_ORIGIN = 'http://localhost:8792';
/** Where approval mails go (ADMIN_NOTIFY_EMAIL in compose.yaml); also the license admin Dex user. */
const ADMIN_NOTIFY = 'admin@example.org';
/** A one-click approval link: 32 random bytes, base64url. */
const APPROVE_LINK = /https?:\/\/\S+\/approve\?token=[A-Za-z0-9_-]{43}/;
const run = Date.now().toString(36);
const company = `Example GmbH ${run}`;

let licenseId = '';

/** A pattern that matches exactly this text. */
function exactly(text: string): RegExp {
  return new RegExp(`^${text.replace(/[.*+?^$()|[\]{}\\]/g, '\\$&')}$`);
}

async function asUser(browser: Browser, email: string): Promise<Page> {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 860 } })).newPage();
  await signIn(page, email);
  return page;
}

/** A personal key request as the person makes it. */
interface PersonalRequest {
  name: string;
  email: string;
  use: string;
}

/**
 * The approval mail of a personal request to ADMIN_NOTIFY_EMAIL: the
 * details and the recipient's own one-click link, before anything is
 * signed. Returns the link.
 */
async function approvalLink(request: PersonalRequest): Promise<string> {
  const mail = await waitForMail(ADMIN_NOTIFY, exactly(`Approve a free personal key for ${request.name}?`));
  expect(mail.text).toContain(request.email);
  expect(mail.text).toContain(request.use);
  expect(mail.attachments).toEqual([]);
  const link = APPROVE_LINK.exec(mail.text)?.[0];
  if (!link) throw new Error(`the approval mail for ${request.name} has no one-click link`);
  // Nothing is signed before an admin decides.
  expect((await listMail(request.email)).filter((m) => m.Attachments > 0)).toEqual([]);
  return link;
}

/**
 * Opens a one-click link in a fresh browser with no session (the link is
 * the credential): the page shows the request and changes nothing until a
 * button is pressed.
 */
async function openApprovalPage(browser: Browser, link: string, request: PersonalRequest): Promise<Page> {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 860 } })).newPage();
  await page.goto(link);
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
  await expect(page.getByText(request.name).first()).toBeVisible();
  await expect(page.getByText(request.use).first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Reject' })).toBeVisible();
  return page;
}

/** An admin approves a personal request from the approval mail; the link works once. */
async function approveFromMail(browser: Browser, request: PersonalRequest, shotName: string): Promise<void> {
  const page = await openApprovalPage(browser, await approvalLink(request), request);
  await shot(page, shotName);
  const approve = page.getByRole('button', { name: 'Approve and send the key' });
  await approve.click();
  await expect(page.getByText(/approved/i).first()).toBeVisible();
  await expect(approve).toHaveCount(0);

  // Opened again, the used link only shows the decision.
  await page.reload();
  await expect(page.getByText(/already approved/i).first()).toBeVisible();
  await expect(page.getByText(request.name).first()).toBeVisible();
  await expect(approve).toHaveCount(0);
  await page.context().close();
}

/** Asks for a free personal key in the portal, signed in as the person; the request waits for approval. */
async function requestPersonalKey(page: Page, request: PersonalRequest, shotName?: string): Promise<void> {
  await page.goto('/my/licenses');
  await page.getByRole('button', { name: 'Request a free personal key' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: 'Name on the key' }).fill(request.name);
  await dialog.getByRole('textbox', { name: 'What do you run kudurru for?' }).fill(request.use);
  await dialog.getByRole('checkbox', { name: /I agree and certify/ }).check();
  if (shotName) await shot(page, shotName);
  await dialog.getByRole('button', { name: 'Request key' }).click();
  await expect(dialog.getByText(/waiting for approval/i).first()).toBeVisible();
}

test.beforeAll(async () => {
  await clearMail();
});

test('downloads requires sign-in and opens after authentication', async ({ page }) => {
  await page.goto('/downloads');
  await expect(page).toHaveURL(/\/login\?next=%2Fdownloads$/);
  await expect(page.getByRole('heading', { name: 'Downloads', exact: true })).toHaveCount(0);

  // Continue from the redirect so the test exercises its original return URL.
  await page.getByRole('link', { name: /^Sign in with/ }).click();
  await page.waitForURL(/\/dex\//);
  await page.locator('input[name="login"]').fill('customer@example.org');
  await page.locator('input[name="password"]').fill(PASSWORD);
  await page.locator('button[type="submit"]').click();
  await expect(page).toHaveURL(/\/downloads$/);
  await expect(page.getByRole('heading', { level: 1, name: 'Downloads' })).toBeVisible();
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

test('the customer accepts the invite, sees the key and asks for a free personal key that an admin approves', async ({ browser }) => {
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

  // The free personal key from the portal: a request that waits for an
  // admin's approval.
  const request = { name: 'Chris Customer', email: 'customer@example.org', use: 'DNS for my home network' };
  await requestPersonalKey(page, request, '05-personal-request');
  await page.context().close();
  await approveFromMail(browser, request, '06-approve-portal-request');
  const mail = await waitForKeyMail(request.email);
  const node = nodeStatus(mail.key.trim());
  if (node) expect(node).toMatchObject({ state: 'active', plan: 'personal', licensee: request.name, nodes: 3, features: expect.arrayContaining(['oidc', 'rbac']) });
});

/**
 * Posts as the kudurru.io form does (www/src/client/license.ts): from a
 * minimal stand-in page on its own origin, served by a real local server
 * (Chrome's Local Network Access refuses a page it sees as public calling a
 * loopback portal), cross-origin. Returns the status and the JSON answer.
 */
async function postFromForm(page: Page, path: string, body: Record<string, unknown>): Promise<{ status: number; body: unknown }> {
  const site = createServer((_req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end('<!doctype html><title>kudurru.io</title><p>the license page</p>');
  });
  await new Promise<void>((done) => site.listen(Number(new URL(FORM_ORIGIN).port), '127.0.0.1', done));
  await test.info().attach('form origin', { body: FORM_ORIGIN });
  try {
    await page.goto(`${FORM_ORIGIN}/license`);
    const portal = new URL(process.env.E2E_BASE_URL ?? 'http://localhost:8791').origin;
    return await page.evaluate(
      async ({ url, body }) => {
        const res = await fetch(url, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'omit',
          body: JSON.stringify(body),
        });
        return { status: res.status, body: (await res.json()) as unknown };
      },
      { url: `${portal}${path}`, body },
    );
  } finally {
    site.close();
  }
}

test('the kudurru.io form: a cross-origin request, a confirmation link, an admin approves, the key by mail', async ({ page, browser }) => {
  const request = { name: 'Homelab Hank', email: `homelab-${run}@example.net`, use: 'DNS for the house' };
  const answer = await postFromForm(page, '/api/v1/public/key-requests', { ...request, noncommercial: true, website: '' });
  expect(answer).toEqual({ status: 202, body: { status: 'sent' } });

  const confirm = await waitForMail(request.email, /confirm/i);
  expect(confirm.text.startsWith('Hello,')).toBe(true);
  const link = /https?:\/\/\S+\/verify\?token=[A-Za-z0-9_-]{43}/.exec(confirm.text)![0];
  await page.goto(link);
  await shot(page, '07-verify');
  await page.getByRole('button', { name: /^Confirm/ }).click();
  await expect(page.getByText(/waiting for approval/i).first()).toBeVisible();

  await approveFromMail(browser, request, '08-approve-form-request');
  const keyMail = await waitForKeyMail(request.email);
  expect(/^[\x20-\x7e]+\n$/.test(keyMail.key)).toBe(true);
  expect(keyMail.text).toContain('community-development use only');
  // No portal account yet: the key mail brings the invite link.
  expect(keyMail.text).toMatch(/\/invites\/[A-Za-z0-9_-]{43}/);
  const node = nodeStatus(keyMail.key.trim());
  if (node) expect(node).toMatchObject({ state: 'active', plan: 'personal', licensee: request.name, nodes: 3 });
});

test('a license admin rejects a request from the approval mail, and the requester gets the note', async ({ browser }) => {
  const request = { name: 'Olive Other', email: 'other@example.org', use: `Name service for a shop ${run}` };
  const note = 'A shop is a business: write to us about a business license.';
  const person = await asUser(browser, request.email);
  await requestPersonalKey(person, request);
  await person.context().close();

  const page = await openApprovalPage(browser, await approvalLink(request), request);
  await page.getByRole('button', { name: 'Reject' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: /Note to the requester/ }).fill(note);
  await dialog.getByRole('button', { name: 'Reject and send note' }).click();
  await expect(page.getByText(/rejected/i).first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Approve and send the key' })).toHaveCount(0);
  await page.context().close();

  const mail = await waitForMail(request.email, exactly('Your request for a kudurru personal key'));
  expect(mail.text).toContain(note);
  expect((await listMail(request.email)).filter((m) => m.Attachments > 0)).toEqual([]);
});

test('the kudurru.io form: a business request, mail to the requester and the admins, a license issued from it', async ({ page, browser }) => {
  const request = { name: 'Ada Admin', email: `business-${run}@example.net`, company: `Business Co ${run}`, edition: 'enterprise', nodes: 12, details: 'OIDC and edge nodes.' };
  const answer = await postFromForm(page, '/api/v1/public/business-requests', { ...request, website: '' });
  expect(answer).toEqual({ status: 202, body: { status: 'sent' } });

  const received = await waitForMail(request.email, exactly('We have your kudurru business license request'));
  expect(received.text).toContain('Enterprise');
  expect(received.text).not.toContain(request.company);
  const notice = await waitForMail(ADMIN_NOTIFY, exactly(`Business license request from ${request.company}`));
  expect(notice.text).toContain('/requests?tab=business');

  const admin = await asUser(browser, 'admin@example.org');
  await admin.goto('/requests?tab=business');
  await expect(admin.getByText(request.company).first()).toBeVisible();
  await shot(admin, '13-business-requests');
  await admin.getByRole('button', { name: `Issue a license for ${request.company}` }).click();
  const wizard = admin.getByRole('form', { name: 'Issue license' });
  await expect(wizard.getByRole('textbox', { name: /Customer name/ })).toHaveValue(request.company);
  for (const step of ['Plan and limits', 'Term', 'Review']) {
    await wizard.getByRole('button', { name: 'Next' }).click();
    await expect(wizard.getByRole('heading', { level: 3, name: step })).toBeVisible();
  }
  await expect(wizard.getByText(`${request.company} (${request.email}): Enterprise, 12 nodes.`, { exact: false })).toBeVisible();
  await wizard.getByRole('button', { name: 'Sign and issue' }).click();
  await admin.waitForURL(/\/licenses\/lic_/);
  await admin.goto('/requests?tab=business&status=issued');
  await expect(admin.getByText(request.company).first()).toBeVisible();
  await admin.context().close();
});

test('the superadmin sees users, the audit trail and the system page', async ({ browser }) => {
  const page = await asUser(browser, 'superadmin@example.org');
  await page.goto('/users');
  await expect(page.getByText('customer@example.org').first()).toBeVisible();
  await shot(page, '09-users');
  await page.goto('/audit');
  await expect(page.getByRole('cell', { name: 'license.issued' }).first()).toBeVisible();
  await expect(page.getByRole('cell', { name: 'request.approved' }).first()).toBeVisible();
  await shot(page, '10-audit');
  await page.goto('/system');
  await expect(page.getByText('dev-1').first()).toBeVisible();
  await expect(page.getByText('Approval', { exact: true }).first()).toBeVisible();
  await expect(page.getByText(/^Approval required/).first()).toBeVisible();
  await expect(page.getByText(ADMIN_NOTIFY, { exact: true }).first()).toBeVisible();
  await shot(page, '11-system');
  await page.goto(`/licenses/${licenseId}`);
  await shot(page, '12-license-superadmin');

  // Delete a user: the account goes, and the list no longer has it.
  await page.goto('/users');
  await page.getByRole('row', { name: /other@example\.org/ }).getByRole('link').click();
  await page.getByRole('button', { name: 'Delete user' }).click();
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete user' }).click();
  await expect(page).toHaveURL(/\/users$/);
  await expect(page.getByText('customer@example.org').first()).toBeVisible();
  await expect(page.getByText('other@example.org')).toHaveCount(0);
  await page.context().close();
});
