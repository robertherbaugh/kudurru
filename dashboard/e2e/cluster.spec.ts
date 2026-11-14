// The cluster membership wizards against a real SQLite node of its own:
// activate the dormant standalone node, restart it as the primary, issue an
// invitation that the dashboard shows exactly once, and prove that the shown
// invitation joins a new member with the offline command. The invitation
// must appear in no console message, request URL, browser storage, server
// log or page after its dialog closed.
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { BrowserContext } from '@playwright/test';
import { env, expect, test } from './fixtures';
import { ADMIN_USER, freePort, startServer } from './support/kudurru';

/** Signs the context in through the API (its cookie jar is the page's). Only
 * the pre-handler publication gate is waited out; any other refusal fails. */
async function signIn(context: BrowserContext, password: string) {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const login = await context.request.post('/api/v1/auth/login', { data: { username: ADMIN_USER, password } });
    if (login.ok()) return;
    const key = ((await login.json().catch(() => ({}))) as { error?: { key?: string } }).error?.key;
    if (login.status() !== 503 || key !== 'generation_pending' || Date.now() > deadline) throw new Error(`login failed: HTTP ${login.status()} ${key ?? ''}`);
    await new Promise((r) => setTimeout(r, 500));
  }
}

test('activates a primary, shows an invitation exactly once and the invitation joins a member', async ({ browser }) => {
  test.setTimeout(180_000);
  const bin = env('KUDURRU_E2E_BIN_PATH');
  const primary = await startServer(bin, { node: 'e2e-cluster-a', upstream: env('KUDURRU_E2E_UPSTREAM'), adminPassword: 'cluster-e2e-password' });
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'kudurru-e2e-cluster-'));
  // A browser context of its own: the worker's session cookie is for another node.
  const context = await browser.newContext({ baseURL: primary.url, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  const consoleText: string[] = [];
  const urls: string[] = [];
  page.on('console', (m) => consoleText.push(m.text()));
  page.on('request', (r) => urls.push(r.url()));
  page.on('framenavigated', (f) => urls.push(f.url()));
  try {
    await signIn(context, primary.adminPassword);
    await page.goto('/nodes');
    const membership = page.getByRole('region', { name: 'Cluster membership' });
    await expect(membership.getByText(/This node is not in a cluster/)).toBeVisible();

    // Activate: review, member, activate.
    await membership.getByRole('button', { name: 'Activate cluster' }).click();
    const activate = page.getByRole('dialog', { name: 'Activate cluster' });
    await expect(activate.getByText('standalone (dormant)')).toBeVisible();
    await activate.getByRole('checkbox', { name: /I reviewed this node's enrollment/ }).check();
    await activate.getByRole('button', { name: 'Next' }).click();
    await expect(activate.getByRole('textbox', { name: /^Member name/ })).toHaveValue('e2e-cluster-a');
    await activate.getByRole('textbox', { name: /^Internal address/ }).fill(primary.internal);
    await activate.getByRole('textbox', { name: /^Reason/ }).fill('dashboard e2e activation');
    await activate.getByRole('button', { name: 'Next' }).click();
    await activate.getByRole('button', { name: 'Activate cluster' }).click();
    await expect(membership.getByText('Cluster activated: restart kudurru on this node')).toBeVisible();
    await expect(activate).toBeHidden();

    await primary.restart();
    // The session is kept in the node's database across the restart.
    expect((await context.request.get('/api/v1/auth/me')).status()).toBe(200);
    await page.goto('/nodes');
    await expect(membership.getByText(/This node is the primary/)).toBeVisible({ timeout: 20_000 });

    // The root trust anchors panel renders this real node's GET /cluster/anchors answer.
    const anchors = page.getByRole('region', { name: 'Root trust anchors' });
    await expect(anchors.getByText('Anchor mode')).toBeVisible();
    await expect(anchors.getByText('Authority')).toBeVisible();
    await expect(anchors.getByText('No trust-anchor state needs you on this node.').or(anchors.getByRole('list', { name: 'Trust-anchor states that need you' }))).toBeVisible();
    await expect(anchors.getByRole('alert')).toHaveCount(0);

    // Invite: review, new member, issue.
    const target = { name: 'e2e-cluster-b', address: `127.0.0.1:${await freePort()}` };
    await membership.getByRole('button', { name: 'Invite a member' }).click();
    const invite = page.getByRole('dialog', { name: 'Invite a member' });
    await expect(invite.getByText('primary', { exact: true })).toBeVisible();
    await invite.getByRole('checkbox', { name: /I reviewed the primary's enrollment/ }).check();
    await invite.getByRole('button', { name: 'Next' }).click();
    await invite.getByRole('textbox', { name: /^Member name/ }).fill(target.name);
    await invite.getByRole('textbox', { name: /^Internal address/ }).fill(target.address);
    await invite.getByRole('textbox', { name: /^Reason/ }).fill('dashboard e2e invitation');
    await invite.getByRole('button', { name: 'Next' }).click();
    await expect(invite.getByText('Shown once')).toBeVisible();
    await invite.getByRole('button', { name: 'Issue invitation' }).click();
    const shown = invite.getByTestId('cluster-invitation');
    await expect(shown).toBeVisible();
    const invitation = (await shown.locator('code, pre').first().innerText()).trim();
    expect(invitation.length).toBeGreaterThan(64);
    await expect(invite.getByText(`--confirm-name ${target.name}`, { exact: false }).first()).toBeVisible();

    // Shown once: Done drops it, and a new wizard starts at a new review.
    await invite.getByRole('button', { name: 'Done' }).click();
    await expect(invite).toBeHidden();
    expect(await page.content()).not.toContain(invitation);
    await membership.getByRole('button', { name: 'Invite a member' }).click();
    await expect(invite.getByRole('checkbox', { name: /I reviewed the primary's enrollment/ })).not.toBeChecked();
    expect(await page.content()).not.toContain(invitation);
    await invite.getByRole('button', { name: 'Cancel' }).click();
    await expect(invite).toBeHidden();

    // Never in a console message, a URL, browser storage or the server log.
    const storage = await page.evaluate(() => JSON.stringify([{ ...window.localStorage }, { ...window.sessionStorage }, document.cookie]));
    for (const [where, text] of [['console', consoleText.join('\n')], ['urls', urls.join('\n')], ['storage', storage], ['server log', primary.log()]] as const) {
      expect(text.includes(invitation), `invitation found in ${where}`).toBe(false);
    }

    // The shown invitation joins the new member (one offline command, never repeated).
    const file = path.join(scratch, 'member.invitation');
    fs.writeFileSync(file, invitation, { mode: 0o600 });
    const join = spawnSync(bin, ['cluster', 'join', '--offline', '--data-dir', path.join(scratch, 'member'), '--invitation-file', file,
      '--confirm-name', target.name, '--confirm-address', target.address], { env: primary.env, encoding: 'utf8', timeout: 40_000 });
    expect(join.status, 'cluster join exit status').toBe(0);
    const result = JSON.parse(join.stdout) as { primary?: { committed?: boolean }; installed?: { digest?: string } };
    expect(result.primary?.committed).toBe(true);
    expect(result.installed?.digest).toBeTruthy();
  } finally {
    await context.close();
    await primary.stop();
    fs.rmSync(scratch, { recursive: true, force: true });
  }
});
