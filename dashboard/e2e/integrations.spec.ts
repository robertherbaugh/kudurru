// The Integrations page against a real node and a local provider: a node
// whose credentials.allowedFiles grants one bearer-token file, and an
// external-dns webhook peer on 127.0.0.1 (support/externaldns.ts). The
// webhook Integration is created through the manifest-driven wizard with a
// file reference (the token itself is never typed into the browser), the
// host publishes the zone's records with its registry rows, and after the
// peer loses them a reviewed preview shows the plan and its digest and
// "Publish this plan" restores exactly that. Then check, and delete. A
// second instance with deletionPolicy Delete is edited in place, then
// deleted while its peer refuses writes: it stays terminating on its page
// until "Release without cleanup" removes it and leaves the peer's rows.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { randomBytes } from 'node:crypto';
import type { Page } from '@playwright/test';
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

  const signIn = async (page: Page) => {
    await page.goto(server.url);
    await page.getByRole('textbox', { name: 'Username' }).fill(ADMIN_USER);
    await page.getByLabel(/^Password/).fill(password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(main(page).getByRole('heading', { name: 'Dashboard' })).toBeVisible();
    await settle(page);
  };

  // Each status write gives the object a new revision, and a command is
  // bound to the revision on screen: review once it is stable.
  const steady = async (page: Page, name: string) => {
    let last = '';
    await expect
      .poll(async () => {
        const r = (await (await page.request.get(`${server.url}/api/v1/integrations/default/${name}`)).json()) as { revision?: string };
        const same = !!r.revision && r.revision === last;
        last = r.revision ?? '';
        return same;
      }, { timeout: 60_000, intervals: [3_000] })
      .toBe(true);
    await page.reload();
    await settle(page);
  };

  test('creates a webhook integration, previews and publishes the reviewed plan', async ({ page }) => {
    test.setTimeout(180_000);
    await signIn(page);

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

    await steady(page, 'records');
    const preview = main(page).getByRole('button', { name: 'Preview' });
    await expect(preview).not.toHaveAttribute('aria-disabled', 'true');
    // The peer loses them: a reviewed preview plans their return.
    peer.wipe();
    const before = peer.posts();
    await preview.click();
    const plan = page.getByTestId('integration-preview');
    await expect(plan).toBeVisible({ timeout: 30_000 });
    await expect(plan).toContainText(/Plan digest [0-9a-f]{64}/);
    await expect(plan.getByRole('table', { name: 'Planned changes' })).toContainText(`${ZONE.host}. A`);
    expect(peer.posts()).toBe(before);
    await main(page).getByRole('button', { name: 'Publish this plan' }).click();
    await expect(page.getByTestId('command-outcome')).toContainText('Sync completed', { timeout: 30_000 });
    expect(peer.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);
    expect(peer.posts()).toBeGreaterThan(before);

    // Check reaches the peer with the referenced credential.
    await steady(page, 'records');
    await main(page).getByRole('button', { name: 'Check' }).click();
    await expect(page.getByTestId('command-outcome')).toContainText('Reachable: yes. Credential accepted: yes.', { timeout: 30_000 });

    // Delete (Retain): the object goes, the records stay at the peer.
    await steady(page, 'records');
    await main(page).getByRole('button', { name: 'Delete' }).click();
    await page.getByRole('alertdialog').getByRole('button', { name: 'Delete integration' }).click();
    await expect(page).toHaveURL(/\/integrations$/);
    await expect(main(page).getByText('No integrations yet')).toBeVisible();
    expect(peer.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);
  });

  test('edits a Delete instance, holds it terminating and releases it without cleanup', async ({ page }) => {
    test.setTimeout(240_000);
    const held = await startExternalDNSPeer(token, [ZONE.object]);
    try {
      await signIn(page);
      await page.goto(`${server.url}/integrations?new=1`);
      await settle(page);
      const dialog = page.getByRole('dialog');
      const next = () => dialog.getByRole('button', { name: 'Next' }).click();
      await dialog.getByRole('radio', { name: /Webhook/ }).check();
      await next();
      await dialog.getByRole('textbox', { name: /^Name(?!space)/ }).fill('cleanup');
      await dialog.getByRole('textbox', { name: /^Endpoint/ }).fill(held.url);
      await dialog.getByRole('group', { name: /^Domains/ }).getByRole('button', { name: 'Add', exact: true }).click();
      await dialog.getByRole('textbox', { name: /^Domains.* 1$/ }).fill(ZONE.object);
      await choose(selector(dialog, /^When deleted/), 'Delete');
      await next();
      await choose(selector(dialog, /^Bearer token reference/), 'File on the node');
      await choose(selector(dialog, /^Bearer token file/), tokenFile);
      await next();
      await dialog.getByRole('button', { name: 'Create integration' }).click();
      await expect(page).toHaveURL(/\/integrations\/default\/cleanup$/);
      await expect.poll(() => held.rows()[`${ZONE.host}/A`], { timeout: 60_000 }).toEqual([ZONE.hostA]);

      // A configuration edit is a PUT bound to the stored resourceVersion;
      // the credential reference and the deletion policy are kept.
      const stored = async () => (await (await page.request.get(`${server.url}/api/v1/objects/integrations/default/cleanup`)).json()) as { spec?: { suspend?: boolean; deletionPolicy?: string; credentials?: unknown } };
      await steady(page, 'cleanup');
      await main(page).getByRole('button', { name: 'Edit Configuration' }).click();
      await main(page).getByRole('switch', { name: /Suspended/ }).click();
      await main(page).getByRole('button', { name: 'Save Configuration' }).click();
      await expect.poll(async () => (await stored()).spec?.suspend, { timeout: 30_000 }).toBe(true);
      expect((await stored()).spec?.deletionPolicy).toBe('Delete');
      expect((await stored()).spec?.credentials).toEqual({ bearerToken: { file: tokenFile } });
      await steady(page, 'cleanup');
      await main(page).getByRole('button', { name: 'Edit Configuration' }).click();
      await main(page).getByRole('switch', { name: /Suspended/ }).click();
      await main(page).getByRole('button', { name: 'Save Configuration' }).click();
      await expect.poll(async () => (await stored()).spec?.suspend ?? false, { timeout: 30_000 }).toBe(false);

      // The peer refuses writes, so the cleanup cannot remove the records:
      // the instance stays on its page as terminating.
      await steady(page, 'cleanup');
      held.refuseWrites(true);
      await main(page).getByRole('button', { name: 'Delete' }).click();
      await expect(page.getByRole('alertdialog')).toContainText(/Its deletion policy is Delete/);
      await page.getByRole('alertdialog').getByRole('button', { name: 'Delete integration' }).click();
      const panel = page.getByTestId('terminating');
      await expect(panel).toBeVisible({ timeout: 30_000 });
      await expect(page).toHaveURL(/\/integrations\/default\/cleanup$/);
      await expect(main(page).getByRole('button', { name: 'Preview' })).toHaveCount(0);
      await expect(main(page).getByRole('button', { name: 'Edit Configuration' })).toHaveAttribute('aria-disabled', 'true');
      expect(held.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);

      // Release without cleanup removes the object and leaves the rows.
      await steady(page, 'cleanup');
      const release = page.getByTestId('terminating').getByRole('button', { name: 'Release without cleanup' });
      await expect(release).not.toHaveAttribute('aria-disabled', 'true');
      await release.click();
      await page.getByRole('alertdialog').getByRole('button', { name: 'Release without cleanup' }).click();
      await expect(page).toHaveURL(/\/integrations$/);
      await expect.poll(async () => (await page.request.get(`${server.url}/api/v1/objects/integrations/default/cleanup`)).status(), { timeout: 30_000 }).toBe(404);
      expect(held.rows()[`${ZONE.host}/A`]).toEqual([ZONE.hostA]);
    } finally {
      await held.close();
    }
  });
});
