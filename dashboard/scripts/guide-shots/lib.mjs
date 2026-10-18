// Shared helpers of the guide screenshot script (../guide-screenshots.mjs).
import { chromium } from 'playwright-core';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const OUT = process.env.KUDURRU_GUIDE_OUT ? path.resolve(process.env.KUDURRU_GUIDE_OUT) : path.resolve(here, '../../../docs/guide/images');
const BASE = process.env.KUDURRU_GUIDE_URL ?? 'http://localhost:5199';
export const BLOCKPAGE = process.env.KUDURRU_GUIDE_BLOCKPAGE;
export const CHROME = process.env.KUDURRU_GUIDE_CHROME ?? '/usr/bin/google-chrome';
const ONLY = process.env.KUDURRU_GUIDE_ONLY?.split(',').filter(Boolean);
export const GROUPS = process.env.KUDURRU_GUIDE_GROUPS?.split(',').filter(Boolean);
export const W = 1280;
export const H = 1000;

fs.mkdirSync(OUT, { recursive: true });

export const browser = await chromium.launch({ executablePath: CHROME, args: ['--no-sandbox'] });
let page;

/** A new context: the mock database lives in the page, so every group of shots starts clean. */
export async function fresh(user = 'admin', { dismissBanner = true, height = H } = {}) {
  const ctx = await browser.newContext({ viewport: { width: W, height }, serviceWorkers: 'allow' });
  page = await ctx.newPage();
  page.on('pageerror', (e) => console.log('  pageerror:', e.message));
  await page.goto(`${BASE}/${user ? `?mockUser=${user}` : ''}`);
  await settle();
  if (user && dismissBanner) await dismiss();
}

export async function settle(ms = 1500) {
  await page.waitForTimeout(ms);
}

/** Closes the "default password" banner so that shots show the page itself. */
export async function dismiss() {
  const close = page.getByRole('button', { name: /^(Close|Dismiss)/ }).first();
  if (await close.count()) await close.click().catch(() => {});
  await page.waitForTimeout(200);
}

/** In-app navigation keeps the mock database (a reload signs out). */
export async function go(route) {
  await page.evaluate((r) => {
    history.pushState({}, '', r);
    dispatchEvent(new PopStateEvent('popstate'));
  }, route);
  await settle();
  await toTop();
}

/** Scrolls every scrolled element back to the top. */
export async function toTop() {
  await page.evaluate(() => {
    for (const el of document.querySelectorAll('*')) if (el.scrollTop > 0) el.scrollTop = 0;
    window.scrollTo(0, 0);
  });
  await page.waitForTimeout(200);
}

/** Scrolls the page content by a number of pixels (the scroller is not always the window). */
export async function scroll(dy) {
  await page.mouse.move(700, 500);
  await page.mouse.wheel(0, dy);
  await page.waitForTimeout(400);
}

export const main = () => page.getByRole('main');
export const wanted = (name) => !ONLY || ONLY.includes(name);

export async function shot(name, { clip, locator, fullPage } = {}) {
  if (!wanted(name)) return;
  await page.mouse.move(W - 4, H - 4); // no stray tooltip in the shot
  await page.waitForTimeout(250);
  const file = path.join(OUT, `${name}.png`);
  // A dialog is shown alone: the blurred page behind it adds size and noise, not information.
  const open = page.getByRole('dialog').last();
  const target = locator ?? ((await open.count()) && (await open.isVisible()) ? open : null);
  if (target) await target.screenshot({ path: file });
  else await page.screenshot({ path: file, clip, fullPage });
  console.log('shot', name);
}

/** Runs one group of shots; a failing group is reported and does not stop the others. */
export async function group(title, fn) {
  if (GROUPS && !GROUPS.includes(title)) return;
  console.log(`# ${title}`);
  try {
    await fn();
  } catch (err) {
    console.log(`  FAILED ${title}: ${err.message.split('\n')[0]}`);
    process.exitCode = 1;
  }
  await page?.context().close();
}


export const getPage = () => page;

/** Opens a Selector (combobox) and picks the option with this name. */
export async function choose(trigger, option) {
  await trigger.click();
  const id = await trigger.getAttribute('aria-controls');
  const list = id ? page.locator(`[id="${id}"]`) : page.getByRole('listbox');
  await list.getByRole('option', { name: option, exact: typeof option === 'string' }).click();
  await page.waitForTimeout(500);
}

/** The dialog that is open now. */
export const dialog = () => page.getByRole('dialog');
