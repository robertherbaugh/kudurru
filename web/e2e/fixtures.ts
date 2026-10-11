// Shared fixtures of the end-to-end suite. Every test runs against the
// server of global-setup.ts (baseURL), signed in as the admin (storage
// state), and fails on uncaught page exceptions and console errors unless
// the test allows them (allowConsole).
import { expect, test as base, type Locator, type Page } from '@playwright/test';

/** A value global-setup.ts exported through the environment. */
export function env(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set: the suite needs global-setup.ts (run it with \`bun run e2e\`)`);
  return v;
}

interface Fixtures {
  /** Console errors matching this are not failures (null: none allowed). */
  allowConsole: RegExp | null;
  /** Console errors and uncaught exceptions of the page (checked after the test). */
  pageProblems: string[];
}

/** The first lines of a message or stack (minified frames say little). */
const head = (s: string) => s.split('\n').slice(0, 3).join('\n');

export const test = base.extend<Fixtures>({
  baseURL: async ({}, use) => {
    await use(env('KUDURRU_E2E_URL'));
  },
  allowConsole: [null, { option: true }],
  pageProblems: [
    async ({ page, allowConsole }, use) => {
      const problems: string[] = [];
      page.on('pageerror', (err) => problems.push(`uncaught exception on ${page.url()}: ${head(err.stack ?? err.message)}`));
      page.on('console', (msg) => {
        if (msg.type() !== 'error') return;
        const loc = msg.location();
        const text = `${head(msg.text())}${loc.url ? ` (${loc.url})` : ''}`;
        if (allowConsole?.test(text)) return;
        problems.push(`console error on ${page.url()}: ${text}`);
      });
      await use(problems);
      expect(problems, 'console errors or uncaught exceptions').toEqual([]);
    },
    { auto: true },
  ],
});

export { expect };

/** The routed page content: the shell's main landmark. */
export function main(page: Page): Locator {
  return page.locator('#astryx-app-shell-main');
}

/** Literal leftovers of a field the SPA read under the wrong name or type. */
export const BAD_TEXT = /\bundefined\b|\bNaN\b|\[object Object\]/;

/** The lines of a text that show undefined, NaN or [object Object]. */
export function badLines(text: string): string[] {
  return text.split('\n').filter((l) => BAD_TEXT.test(l));
}

/**
 * Waits for the page's first queries: its heading is visible and no
 * spinner (an Astryx Spinner is a status named by its label) is left.
 * networkidle is no use here: the query log keeps an EventSource open.
 */
export async function settle(page: Page): Promise<void> {
  const crashed = page.getByText('This page failed to render');
  await expect(main(page).getByRole('heading', { level: 1 }).first().or(crashed)).toBeVisible();
  if (await crashed.count()) {
    // Fail at once with the error message the boundary shows.
    expect((await main(page).innerText()).replace(/\s+/g, ' ').trim(), 'the ErrorBoundary caught a render error').not.toContain('This page failed to render');
  }
  await expect(page.getByRole('status', { name: /^(Loading|Connecting|Checking)/ })).toHaveCount(0);
}

/**
 * Asserts the page rendered: no ErrorBoundary, no "Page not found", no
 * failed query banner and no undefined/NaN/[object Object] in the main
 * content.
 */
export async function expectRendered(page: Page): Promise<void> {
  await expect(page.getByText('This page failed to render'), 'the ErrorBoundary caught a render error').toHaveCount(0);
  await expect(main(page).getByRole('heading', { name: 'Page not found' }), 'no route for this path').toHaveCount(0);
  await expect(main(page).getByText('Something went wrong'), 'a query of the page failed (ErrorBanner)').toHaveCount(0);
  expect(badLines(await main(page).innerText()), 'lines of the main content showing undefined, NaN or [object Object]').toEqual([]);
}

/** The 401 of GET /auth/me before sign-in is expected on the login page. */
export const SIGNED_OUT_401 = /status of 401/;
