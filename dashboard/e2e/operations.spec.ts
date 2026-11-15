// The operations surfaces against a real SQLite node of its own: an operator
// edge block created from the Clients page and revoked from its list (D388),
// the query-log sink panel on Nodes for a configured Loki sink whose address
// never reaches the page (D371), and a role binding created with a zone-group
// scope (D385).
import type { APIRequestContext, BrowserContext } from '@playwright/test';
import { env, expect, main, test } from './fixtures';
import { query } from './support/dns';
import { ADMIN_USER, freePort, startServer } from './support/kudurru';

/** Signs in through the API and returns the session's CSRF token; only the
 * pre-handler publication gate is waited out. */
async function signIn(context: BrowserContext, password: string): Promise<string> {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const login = await context.request.post('/api/v1/auth/login', { data: { username: ADMIN_USER, password } });
    if (login.ok()) return ((await login.json()) as { csrf: string }).csrf;
    const key = ((await login.json().catch(() => ({}))) as { error?: { key?: string } }).error?.key;
    if (login.status() !== 503 || key !== 'generation_pending' || Date.now() > deadline) throw new Error(`login failed: HTTP ${login.status()} ${key ?? ''}`);
    await new Promise((r) => setTimeout(r, 500));
  }
}

async function json<T>(request: APIRequestContext, path: string): Promise<T> {
  const r = await request.get(path);
  expect(r.status(), path).toBe(200);
  return (await r.json()) as T;
}

test('blocks a client at the edge, shows the sink status without its address and binds a zone group', async ({ browser }) => {
  test.setTimeout(180_000);
  const node = await startServer(env('KUDURRU_E2E_BIN_PATH'), { node: 'e2e-ops', upstream: env('KUDURRU_E2E_UPSTREAM'), adminPassword: 'ops-e2e-password' });
  const context = await browser.newContext({ baseURL: node.url, ignoreHTTPSErrors: true, storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  const problems: string[] = [];
  page.on('console', (m) => {
    if (m.type() === 'error') problems.push(m.text());
  });
  try {
    const csrf = await signIn(context, node.adminPassword);
    // A client for the Clients page: one query from 127.0.0.1.
    await query(node.dns, 'example.com');
    await expect.poll(async () => (await json<{ items: { key: string }[] }>(context.request, '/api/v1/clients?window=24h')).items.map((c) => c.key), { timeout: 30_000 }).toContain('127.0.0.1');

    // Edge block from the client row, then revoke from the list.
    await page.goto('/clients');
    await main(page).getByRole('button', { name: 'Block 127.0.0.1 at the edge' }).click();
    const dialog = page.getByRole('dialog', { name: 'Block 127.0.0.1 at the edge' });
    await expect(dialog.getByRole('textbox', { name: /^Address or prefix/ })).toHaveValue('127.0.0.1');
    await dialog.getByRole('textbox', { name: /^Reason/ }).fill('e2e scanner');
    const created = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/edge/blocks');
    await dialog.getByRole('button', { name: 'Block', exact: true }).click();
    expect((await created).status()).toBe(201);
    await expect(dialog).toBeHidden();
    const blocks = main(page).getByRole('table', { name: 'Active edge blocks' });
    await expect(blocks.getByText('127.0.0.1/32')).toBeVisible();
    await expect(blocks.getByText('e2e scanner')).toBeVisible();
    const stored = await json<{ items: { prefix: string; reason?: string }[]; revision: string }>(context.request, '/api/v1/edge/blocks');
    expect(stored.items).toEqual([expect.objectContaining({ prefix: '127.0.0.1/32', reason: 'e2e scanner' })]);
    await blocks.getByRole('button', { name: 'Revoke the edge block of 127.0.0.1/32' }).click();
    const revoke = page.getByRole('dialog', { name: 'Revoke the edge block of 127.0.0.1/32' });
    await revoke.getByRole('button', { name: 'Revoke', exact: true }).click();
    await expect(revoke).toBeHidden();
    await expect(main(page).getByText('No client is blocked at the edge.')).toBeVisible();
    expect((await json<{ items: unknown[] }>(context.request, '/api/v1/edge/blocks')).items).toEqual([]);

    // A Loki sink on a closed port: it fails, and its address stays off the page.
    const closed = await freePort();
    const lokiURL = `http://127.0.0.1:${closed}`;
    const patched = await context.request.patch('/api/v1/settings', {
      headers: { 'Content-Type': 'application/merge-patch+json', 'X-Kudurru-CSRF': csrf },
      data: JSON.stringify({ querylog: { sinks: [{ name: 'e2e-loki', type: 'loki', attempts: 1, flushInterval: '1s', loki: { url: lokiURL } }] } }),
    });
    expect(patched.status(), await patched.text()).toBeLessThan(300);
    await expect.poll(async () => {
      await query(node.dns, 'example.org');
      const body = await json<{ sinks: { name: string; errors: number }[] }>(context.request, '/api/v1/querylog/sinks');
      return body.sinks.find((s) => s.name === 'e2e-loki')?.errors ?? -1;
    }, { timeout: 60_000 }).toBeGreaterThan(0);
    await page.goto('/nodes');
    const sinks = main(page).getByRole('table', { name: 'Query-log sinks' });
    const row = sinks.getByRole('row').filter({ hasText: 'e2e-loki' });
    await expect(row).toBeVisible();
    await expect(row).toContainText('Loki');
    await expect(main(page).getByRole('region', { name: 'Query-log sinks' })).toContainText('e2e-ops');
    expect(await page.locator('body').innerText()).not.toContain(`127.0.0.1:${closed}`);

    // A role binding scoped to a zone group.
    await page.goto('/access?tab=iam');
    await main(page).getByRole('button', { name: 'New role binding' }).click();
    const rb = page.getByRole('dialog', { name: 'New role binding' });
    await rb.getByRole('textbox', { name: /^Name(?!space)/ }).fill('e2e-prod-admins');
    await rb.getByRole('textbox', { name: /^Zone group/ }).fill('prod');
    await rb.getByRole('textbox', { name: /^User name/ }).fill('alice');
    const posted = page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/api/v1/iam/rolebindings');
    await rb.getByRole('button', { name: 'Create', exact: true }).click();
    expect((await posted).status()).toBe(201);
    await expect(rb).toBeHidden();
    const bindings = main(page).getByRole('table', { name: 'Role bindings' });
    await expect(bindings.getByRole('row').filter({ hasText: 'e2e-prod-admins' })).toContainText('zone group prod');
    const listed = await json<{ items: { name: string; scope: { kind: string; name?: string } }[] }>(context.request, '/api/v1/iam/rolebindings');
    expect(listed.items.find((b) => b.name === 'e2e-prod-admins')?.scope).toEqual({ kind: 'zonegroup', name: 'prod' });
    expect(problems).toEqual([]);
  } finally {
    await context.close();
    await node.stop();
  }
});
