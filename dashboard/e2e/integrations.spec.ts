// The Integrations page against a real node and a local provider: a node
// whose credentials.allowedFiles grants one bearer-token file, and an
// external-dns webhook peer on 127.0.0.1 (support/externaldns.ts). The
// webhook Integration is created through the manifest-driven wizard with a
// file reference (the token itself is never typed into the browser), the
// host publishes the zone's records with its registry rows, and after the
// peer loses them a reviewed preview shows the plan and its digest and
// "Publish this plan" restores exactly that. Then check, and delete.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { randomBytes } from 'node:crypto';
import { env, expect, main, selector, settle, SIGNED_OUT_401, test } from './fixtures';
import { choose } from './support/astryx';
import { startExternalDNSPeer, type ExternalDNSPeer } from './support/externaldns';
import { ADMIN_USER, startServer, ZONE, type Server } from './support/kudurru';

test.describe('integrations with a webhook peer', () => {
  let server: Server;
  let peer: ExternalDNSPeer;
  let dir: string;
  let tokenFile: string;
  const password = `integrations-${Date.now()}-password`;
  const token = randomBytes(24).toString('hex');
  test.use({ storageState: { cookies: [], origins: [] }, allowConsole: SIGNED_OUT_401 });

  test.beforeAll(async () => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'kudurru-e2e-integrations-'));
    tokenFile = path.join(dir, 'webhook-token');
    fs.writeFileSync(tokenFile, token, { mode: 0o600 });
    peer = await startExternalDNSPeer(token, [ZONE.object]);
    server = await startServer(env('KUDURRU_E2E_BIN_PATH'), {
      upstream: env('KUDURRU_E2E_UPSTREAM'),
      node: 'e2e-integrations',
      adminPassword: password,
      allowedCredentialFiles: [tokenFile],
    });
  });
  test.afterAll(async () => {
    await server?.stop();
    await peer?.close();
    if (dir) fs.rmSync(dir, { recursive: true, force: true });
  });

  test('creates a webhook integration, previews and publishes the reviewed plan', async ({ page }) => {
    test.setTimeout(180_000);
    await page.goto(server.url);
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();

    await page.goto(`${server.url}/integrations`);
    await settle(page);
    await expect(main(page).getByRole('heading', { name: 'Integrations', level: 1 })).toBeVisible();
    await main(page).getByRole('button', { name: 'New integration' }).click();
    const dialog = page.getByRole('dialog');
    const next = () => dialog.getByRole('button', { name: 'Next' }).click();

    // The providers of this build, from the manifests.
    await dialog.getByRole('radio', { name: /Webhook/ }).check();
    await next();
    await dialog.getByRole('textbox', { name: /^Name(?!space)/ }).fill('records');
    await dialog.getByRole('textbox', { name: /^Endpoint/ }).fill(peer.url);
    await dialog.getByRole('group', { name: /^Domains/ }).getByRole('button', { name: 'Add', exact: true }).click();
    await dialog.getByRole('textbox', { name: /^Domains.* 1$/ }).fill(ZONE.object);
    await next();
    // A reference to the granted file; there is no field for the token.
    await choose(selector(dialog, /^Bearer token reference/), 'File on the node');
    await choose(selector(dialog, /^Bearer token file/), tokenFile);
    await next();
    await expect(dialog.getByTestId('integration-review')).toContainText(`bearerToken: file ${tokenFile}`);
    await dialog.getByRole('button', { name: 'Create integration' }).click();
    await expect(page).toHaveURL(/\/integrations\/default\/records$/);
    expect(await page.content()).not.toContain(token);

    // The host publishes the zone's records and registry rows on its own.
    await expect.poll(() => peer.rows()[`${ZONE.host}/A`], { timeout: 60_000 }).toEqual([ZONE.hostA]);
    expect(Object.keys(peer.rows()).some((k) => k.startsWith('_kudurru-'))).toBe(true);

    // The peer loses them: a reviewed preview plans their return.
    peer.wipe();
    const before = peer.posts();
    const preview = main(page).getByRole('button', { name: 'Preview' });
    await expect(preview).not.toHaveAttribute('aria-disabled', 'true', { timeout: 30_000 });
    await preview.click();
    const plan = page.getByTestId('integration-preview');
    await expect(plan).toBeVisible({ timeout: 30_000 });
    await expect(plan).toContainText(/Plan digest [0-9a-f]{64}/);
    await expect(plan.getByRole('table', { name: 'Planned changes' })).toContainText(`${ZONE.host} A`);
    expect(peer.posts()).toBe(before);
    await main(page).getByRole('button', { name: 'Publish this plan' }).click();
    await expect(page.getByTestId('command-outcome')).toContainText('Sync completed', { timeout: 30_000 });
    expect(peer.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);
    expect(peer.posts()).toBeGreaterThan(before);

    // Check reaches the peer with the referenced credential.
    await main(page).getByRole('button', { name: 'Check' }).click();
    await expect(page.getByTestId('command-outcome')).toContainText('Reachable: yes. Credential accepted: yes.', { timeout: 30_000 });

    // Delete (Retain): the object goes, the records stay at the peer.
    await main(page).getByRole('button', { name: 'Delete' }).click();
    await page.getByRole('alertdialog').getByRole('button', { name: 'Delete integration' }).click();
    await expect(page).toHaveURL(/\/integrations$/);
    await expect(main(page).getByText('No integrations yet')).toBeVisible();
    expect(peer.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);
  });
});
