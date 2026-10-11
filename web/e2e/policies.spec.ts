// The policy, group and client pages against the real server: the policy
// wizard creates a policy whose blocked service the DNS listener then
// blocks, the policy page shows the state of the objects.d policy, the
// group page its matchers, members and policies, the group wizard a new
// group, and the client page which group and policy apply to 127.0.0.1.
//
// GET /policies/{ns}/{name}/state (ux-plan C3) comes with the
// backend-lists-policies stream; until a server serves it the page falls
// back to the state derived from the objects and the browser logs the 404
// of that one request, which is the only console error these tests allow.
import { badLines, env, expect, expectRendered, main, settle, test } from './fixtures';
import { query } from './support/dns';
import { BLOCKLIST } from './support/kudurru';

test.use({ allowConsole: /status of 404.*\/api\/v1\/policies\/[^/]+\/[^/]+\/state/ });

const unique = () => Math.random().toString(36).slice(2, 8);

test('the policy wizard creates a policy that blocks a service', async ({ page }) => {
  const name = `e2e-svc-${unique()}`;
  // Before: the stub upstream answers every A question.
  expect((await query(env('KUDURRU_E2E_DNS'), 'www.tiktok.com')).a).not.toContain('0.0.0.0');

  await page.goto('/policies');
  await settle(page);
  await main(page).getByRole('link', { name: 'New policy' }).click();
  await expect(page).toHaveURL((u) => u.pathname === '/policies/new');
  await settle(page);

  // Who
  await main(page).getByRole('textbox', { name: /^Name/ }).fill(name);
  await main(page).getByRole('checkbox', { name: /^default/ }).check();
  await main(page).getByRole('button', { name: 'Next' }).click();
  // What: a blocked service from the catalogue.
  await expect(main(page).getByRole('heading', { level: 2, name: 'What' })).toBeVisible();
  const tiktok = main(page).getByRole('button', { name: /TikTok/ });
  await tiktok.click();
  await expect(tiktok).toHaveAttribute('aria-pressed', 'true');
  for (const step of ['Safe search', 'How', 'When', 'Review']) {
    await main(page).getByRole('button', { name: 'Next' }).click();
    await expect(main(page).getByRole('heading', { level: 2, name: step })).toBeVisible();
  }
  const review = main(page).getByTestId('policy-review');
  await expect(review).toContainText(name);
  await expect(review).toContainText('TikTok');
  await main(page).getByRole('button', { name: 'Create policy' }).click();

  // The policy page: active, with the service blocked.
  await expect(page).toHaveURL((u) => u.pathname === `/policies/default/${name}`);
  await settle(page);
  await expect(main(page).getByRole('heading', { level: 1, name })).toBeVisible();
  const state = main(page).getByTestId('policy-state');
  await expect(state).toContainText(/Active/);
  await expect(main(page).getByTestId('section-services').getByRole('button', { name: /TikTok/ })).toHaveAttribute('aria-pressed', 'true');
  await expectRendered(page);

  // The DNS listener blocks the service once the snapshot is rebuilt.
  await expect
    .poll(async () => (await query(env('KUDURRU_E2E_DNS'), 'www.tiktok.com')).a, { message: 'www.tiktok.com is blocked', timeout: 20_000 })
    .toContain('0.0.0.0');

  // Clean up through the page: delete with confirmation.
  await main(page).getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete policy' }).click();
  await expect(page).toHaveURL((u) => u.pathname === '/policies');
  await expect(main(page).getByTestId(`row-${name}`)).toHaveCount(0);
});

test('the policies table links to a policy page that shows its state', async ({ page }) => {
  await page.goto('/policies');
  await settle(page);
  const row = main(page).getByTestId('row-e2e-blocking');
  await expect(row).toBeVisible();
  await expect(row).toContainText(BLOCKLIST.name);
  expect(badLines(await row.innerText())).toEqual([]);
  await row.getByRole('link', { name: 'e2e-blocking' }).click();
  await expect(page).toHaveURL((u) => u.pathname === '/policies/default/e2e-blocking');
  await settle(page);

  // objects.d owns it: the same page, read-only, with the reason.
  await expect(main(page).getByText(/Read-only: managed by file:/)).toBeVisible();
  await expect(main(page).getByRole('button', { name: 'Edit Who' })).toHaveAttribute('aria-disabled', 'true');
  const state = main(page).getByTestId('policy-state');
  await expect(state.getByText(/^(Active|Shadowed)$/)).toBeVisible();
  await expect(state).toContainText('Priority');
  await expect(state).toContainText('10');
  await expect(main(page).getByTestId('section-who').getByRole('link', { name: 'default' })).toHaveAttribute('href', '/groups/default/default');
  const lists = main(page).getByTestId('section-what').getByRole('table', { name: 'Lists of the policy' });
  await expect(lists.getByRole('row').filter({ hasText: BLOCKLIST.name })).toContainText('ready');

  await main(page).getByRole('button', { name: 'View as YAML' }).click();
  await expect(page.getByLabel('YAML', { exact: true })).toContainText(`- ${BLOCKLIST.name}`);
  await expectRendered(page);
});

test('the group page shows matchers, recent members and the policies that apply', async ({ page }) => {
  await page.goto('/groups');
  await settle(page);
  await main(page).getByTestId('row-default').getByRole('link', { name: 'default' }).first().click();
  await expect(page).toHaveURL((u) => u.pathname === '/groups/default/default');
  await settle(page);
  await expect(main(page).getByRole('table', { name: 'Matchers' })).toContainText('0.0.0.0/0');
  const seen = main(page).getByRole('table', { name: 'Clients of default' });
  await expect(seen.getByRole('link', { name: '127.0.0.1' })).toHaveAttribute('href', '/clients/127.0.0.1');
  await expect(main(page).getByRole('list', { name: 'Policies of default' }).getByRole('link', { name: 'e2e-blocking' })).toBeVisible();
  await expectRendered(page);
});

test('the group wizard creates a group with typed matchers', async ({ page }) => {
  const name = `e2e-grp-${unique()}`;
  await page.goto('/groups/new');
  await settle(page);
  await main(page).getByRole('textbox', { name: /^Name/ }).fill(name);
  await main(page).getByRole('button', { name: 'Next' }).click();
  await expect(main(page).getByRole('heading', { level: 2, name: 'Members' })).toBeVisible();
  await main(page).getByRole('button', { name: 'Add matcher' }).click();
  await main(page).getByRole('combobox', { name: 'Matcher 1 kind' }).selectOption('cidr');
  const value = main(page).getByRole('textbox', { name: 'Matcher 1 CIDR range' });
  await value.fill('10.99.0.0/33');
  await main(page).getByRole('button', { name: 'Next' }).click();
  await expect(main(page).getByText('Matcher 1: IPv4 prefix length is at most 32')).toBeVisible();
  await value.fill('10.99.0.0/16');
  for (const step of ['Policies', 'Review']) {
    await main(page).getByRole('button', { name: 'Next' }).click();
    await expect(main(page).getByRole('heading', { level: 2, name: step })).toBeVisible();
  }
  await main(page).getByRole('button', { name: 'Create group' }).click();
  await expect(page).toHaveURL((u) => u.pathname === `/groups/default/${name}`);
  await settle(page);
  await expect(main(page).getByRole('table', { name: 'Matchers' })).toContainText('10.99.0.0/16');
  await expectRendered(page);
  await main(page).getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete group' }).click();
  await expect(page).toHaveURL((u) => u.pathname === '/groups');
});

test('the client page names the group and the policy of 127.0.0.1', async ({ page }) => {
  await page.goto('/clients');
  await settle(page);
  await main(page).getByRole('table', { name: /clients/i }).getByRole('link', { name: '127.0.0.1' }).click();
  await expect(page).toHaveURL((u) => decodeURIComponent(u.pathname) === '/clients/127.0.0.1');
  await settle(page);
  await expect(main(page).getByRole('heading', { level: 1, name: '127.0.0.1' })).toBeVisible();
  const groups = main(page).getByTestId('client-groups');
  await expect(groups.getByRole('link', { name: 'default' })).toBeVisible();
  const policy = main(page).getByTestId('client-policy');
  await expect(policy.getByRole('list', { name: 'Candidate policies' }).getByRole('link', { name: 'e2e-blocking' })).toBeVisible();
  await expect(policy).toContainText('decides');
  // The recent queries hold the blocked name of the global setup's traffic.
  await expect(main(page).locator('.k-log')).toContainText(BLOCKLIST.blocked);
  await expectRendered(page);
});

test.describe('dark scheme', () => {
  test.use({ colorScheme: 'dark' });

  test('the policy page and the wizard render', async ({ page }) => {
    for (const path of ['/policies/default/e2e-blocking', '/policies/new']) {
      await page.goto(path);
      await settle(page);
      await expectRendered(page);
    }
  });
});
