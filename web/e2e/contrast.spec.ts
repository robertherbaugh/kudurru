// Hover states keep the text readable: with the pointer on a table row and
// on a stat tile, every piece of text in it has a WCAG contrast ratio of at
// least 4.5 against the background it is painted on (computed in the page
// from the computed styles, in both colour schemes).
import { expect, settle, test } from './fixtures';
import type { Locator, Page } from '@playwright/test';

interface Sample {
  text: string;
  fg: string;
  bg: string;
  ratio: number;
}

/** Contrast of every text node's element under root (runs in the page). */
function measure(root: Element): Sample[] {
  const canvas = document.createElement('canvas');
  canvas.width = canvas.height = 1;
  const ctx = canvas.getContext('2d', { willReadFrequently: true })!;
  // Any CSS colour (rgb, oklch, color-mix ...) to sRGB 0-255 and alpha 0-1.
  const rgba = (css: string): [number, number, number, number] => {
    ctx.clearRect(0, 0, 1, 1);
    ctx.fillStyle = '#000';
    ctx.fillStyle = css;
    ctx.fillRect(0, 0, 1, 1);
    const d = ctx.getImageData(0, 0, 1, 1).data;
    return [d[0]!, d[1]!, d[2]!, d[3]! / 255];
  };
  const over = (top: number[], bottom: number[]): [number, number, number, number] => {
    const a = top[3]! + bottom[3]! * (1 - top[3]!);
    if (a === 0) return [0, 0, 0, 0];
    const c = (i: number) => (top[i]! * top[3]! + bottom[i]! * bottom[3]! * (1 - top[3]!)) / a;
    return [c(0), c(1), c(2), a];
  };
  // What shows through a transparent root: the browser's canvas colour.
  const canvasBg = rgba(matchMedia('(prefers-color-scheme: dark)').matches ? '#121212' : '#fff');
  // The colour behind el: its own and its ancestors' backgrounds composited.
  const background = (el: Element | null): [number, number, number, number] => {
    const layers: [number, number, number, number][] = [];
    for (let e = el; e; e = e.parentElement) {
      const c = rgba(getComputedStyle(e).backgroundColor);
      if (c[3] > 0) layers.push(c);
      if (c[3] >= 1) break;
    }
    let out: [number, number, number, number] = [canvasBg[0], canvasBg[1], canvasBg[2], 1];
    for (const l of layers.reverse()) out = over(l, out);
    return out;
  };
  const lum = (c: number[]) => {
    const ch = (v: number) => {
      const s = v / 255;
      return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    };
    return 0.2126 * ch(c[0]!) + 0.7152 * ch(c[1]!) + 0.0722 * ch(c[2]!);
  };
  const out: Sample[] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const text = n.textContent?.trim();
    const el = n.parentElement;
    if (!text || !el) continue;
    const r = el.getBoundingClientRect();
    const st = getComputedStyle(el);
    if (r.width === 0 || r.height === 0 || st.visibility === 'hidden' || st.display === 'none') continue;
    const bg = background(el);
    const fg = over(rgba(st.color), bg);
    const [l1, l2] = [lum(fg), lum(bg)].sort((a, b) => b - a) as [number, number];
    const fmt = (c: number[]) => `rgb(${c.slice(0, 3).map((v) => Math.round(v)).join(' ')})`;
    out.push({ text: text.slice(0, 40), fg: fmt(fg), bg: fmt(bg), ratio: Math.round(((l1 + 0.05) / (l2 + 0.05)) * 100) / 100 });
  }
  return out;
}

/** Hovers target, waits for its transitions and returns the low-contrast samples. */
async function lowContrastOnHover(page: Page, target: Locator): Promise<Sample[]> {
  await target.hover();
  await page.evaluate(() => Promise.all(document.getAnimations().filter((a) => a.effect?.getComputedTiming().iterations !== Infinity).map((a) => a.finished.catch(() => undefined))));
  const samples = await target.evaluate(measure);
  expect(samples.length, 'text measured').toBeGreaterThan(0);
  return samples.filter((s) => s.ratio < 4.5);
}

for (const colorScheme of ['light', 'dark'] as const) {
  test.describe(`${colorScheme} scheme`, () => {
    test.use({ colorScheme });

    test('a hovered table row keeps 4.5:1 text contrast', async ({ page }) => {
      await page.goto('/lists');
      await settle(page);
      const row = page.locator('#astryx-app-shell-main table tbody tr').first();
      await expect(row).toBeVisible();
      expect(await lowContrastOnHover(page, row)).toEqual([]);
    });

    test('a hovered stat tile keeps 4.5:1 text contrast', async ({ page }) => {
      await page.goto('/');
      await settle(page);
      const tiles = page.getByRole('list', { name: 'Summary' }).getByRole('listitem');
      await expect(tiles).toHaveCount(4);
      for (const tile of await tiles.all()) {
        expect(await lowContrastOnHover(page, tile), `tile ${await tile.getAttribute('aria-label')}`).toEqual([]);
      }
    });
  });
}
