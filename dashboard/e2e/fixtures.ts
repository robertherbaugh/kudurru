// Shared fixtures of the end-to-end suite. Every test runs against the
// worker's server from global-setup.ts (baseURL), signed in as the admin (storage
// state), and fails on uncaught page exceptions and console errors unless
// the test allows them (allowConsole).
import { expect, test as base, type Locator, type Page } from '@playwright/test';
import path from 'node:path';
import { setupServer } from './global-setup';
import { WEB_DIR } from './support/kudurru';

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
  /** Exact GET paths allowed to report the pre-handler publication gate. */
  pendingReadPaths: Set<string>;
}

/** The first lines of a message or stack (minified frames say little). */
const head = (s: string) => s.split('\n').slice(0, 3).join('\n');

export const test = base.extend<Fixtures, { workerState: string }>({
  workerState: [async ({}, use, workerInfo) => {
    const state = path.join(WEB_DIR, 'e2e', '.auth', `admin-worker-${workerInfo.workerIndex}.json`);
    const stop = await setupServer(state, `e2e-worker-${workerInfo.workerIndex}`);
    try { await use(state); } finally { await stop(); }
  }, { scope: 'worker', auto: true, timeout: 60_000 }],
  storageState: async ({ workerState }, use) => {
    await use(workerState);
  },
  baseURL: async ({}, use) => {
    await use(env('KUDURRU_E2E_URL'));
  },
  allowConsole: [null, { option: true }],
  pendingReadPaths: async ({}, use) => { await use(new Set<string>()); },
  pageProblems: [
    async ({ page, allowConsole, pendingReadPaths }, use, testInfo) => {
      const problems: string[] = [];
      const pending: Promise<void>[] = [];
      const confirmed = new Map<string, number>();
      const candidates: { problem: string; url: string }[] = [];
      page.on('response', (response) => {
        const url = new URL(response.url());
        if (response.status() !== 503 || response.request().method() !== 'GET' ||
            url.origin !== new URL(page.url()).origin || url.search || !pendingReadPaths.has(url.pathname)) return;
        pending.push((async () => {
          try {
            const headers = await response.allHeaders();
            const body = await response.body();
            if (body.length > 4096 || headers['set-cookie'] || headers.location) return;
            const value = JSON.parse(body.toString()) as { error?: { key?: unknown } };
            if (value.error?.key === 'generation_pending') confirmed.set(response.url(), (confirmed.get(response.url()) ?? 0) + 1);
          } catch { /* Unreadable or malformed errors remain failures. */ }
        })());
      });
      page.on('pageerror', (err) => problems.push(`uncaught exception on ${page.url()}: ${head(err.stack ?? err.message)}`));
      page.on('console', (msg) => {
        if (msg.type() !== 'error') return;
        const loc = msg.location();
        const text = `${head(msg.text())}${loc.url ? ` (${loc.url})` : ''}`;
        if (allowConsole?.test(text)) return;
        const problem = `console error on ${page.url()}: ${text}`;
        problems.push(problem);
        if (/^Failed to load resource: the server responded with a status of 503 \((?:Service Unavailable)?\)$/.test(msg.text())) {
          candidates.push({ problem, url: loc.url });
        }
      });
      await use(problems);
      await Promise.all(pending);
      // Only a verified middleware refusal on a test-declared read is expected.
      // Handler errors, mutations and unrelated console exceptions still fail.
      let observed = 0;
      for (const { problem, url } of candidates) {
        const count = confirmed.get(url) ?? 0;
        const at = problems.indexOf(problem);
        if (count > 0 && at >= 0) {
          confirmed.set(url, count - 1);
          problems.splice(at, 1);
          observed++;
        }
      }
      if (observed) testInfo.annotations.push({ type: 'publication pending', description: `${observed} verified read refusals; recovery asserted by the test` });
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

/**
 * An Astryx Selector by its label: a combobox, or a button that opens a
 * listbox when the Selector has a search box. SelectField puts the value
 * on the trigger as data-value; pick an option with support/astryx.ts
 * choose().
 */
export function selector(root: Page | Locator, name: string | RegExp): Locator {
  return root.getByRole('combobox', { name }).or(root.getByRole('button', { name }).and(root.locator('[aria-haspopup="listbox"]')));
}

/** The 401 of GET /auth/me before sign-in is expected on the login page. */
export const SIGNED_OUT_401 = /status of 401/;
