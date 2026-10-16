// Browser geometry matters here: jsdom cannot detect clipped table fields.
import type { QueryRow } from '../src/api/types';
import { expect, settle, test } from './fixtures';

test('query log sizes fields to their content and gives remaining space to Domain', async ({ page }) => {
  const domain = `${'long-label.'.repeat(14)}example.com`;
  const rows: QueryRow[] = Array.from({ length: 100 }, (_, i) => ({
    id: 100 - i,
    ts: new Date(Date.now() - i * 1000).toISOString(),
    node: 'kudurru-control-plane-homelab-01',
    proto: 'udp',
    client: { ip: '2001:0db8:1234:5678:90ab:cdef:1234:5678', ...(i % 2 ? { name: 'living-room-television-with-a-long-name' } : {}) },
    domain,
    qtype: 'HTTPS',
    qclass: 1,
    status: i % 2 ? 'blocked_list' : 'dnssec_bogus',
    status_class: i % 2 ? 'blocked' : 'error',
    rcode: 'SERVFAIL',
    reply: { type: 'nodata', time_ms: 12345 },
    dnssec: 'bogus',
    flags: { RD: true, DO: true, TC: false, AD: false, CD: false, Cached: false, Stale: false, Prefetch: false, ECS: false, Traceparent: false },
  }));
  await page.route(/\/api\/v1\/queries(?:\?|$)/, (route) => route.fulfill({
    json: { queries: rows, records_total: rows.length, records_filtered: rows.length },
  }));
  await page.goto('/queries');
  await settle(page);
  const table = page.getByRole('table', { name: 'Query log' });
  const scroller = page.getByTestId('query-log-scroll');
  const first = table.locator('tr[data-row-index="0"]');
  await expect(first).toContainText(rows[0]!.node);

  const widths: number[][] = [];
  for (const width of [1280, 1920, 768]) {
    await page.setViewportSize({ width, height: 900 });
    await expect.poll(() => table.evaluate((t) => {
      // Include nested Text and button labels: a cell can fit while its
      // child silently truncates the content.
      return [...t.querySelectorAll<HTMLElement>('tr[data-row-index] td:not(:nth-child(4))')]
        .flatMap((cell) => [cell, ...cell.querySelectorAll<HTMLElement>('*')])
        .filter((el) => el.clientWidth > 0 && el.scrollWidth > el.clientWidth + 1)
        .map((el) => el.textContent);
    }), { message: `clipped non-domain fields at ${width}px` }).toEqual([]);
    widths.push(await table.locator('th').evaluateAll((cells) => cells.map((c) => c.getBoundingClientRect().width)));
    // The Card owns scrolling in both directions, including when long
    // fields make the table wider than the viewport.
    const nestedScrollers = await table.evaluate((t) => {
      const found: string[] = [];
      for (let el = t.parentElement; el && !el.dataset.testid; el = el.parentElement) {
        if (['auto', 'scroll', 'hidden'].includes(getComputedStyle(el).overflowX)) found.push(el.tagName);
      }
      return found;
    });
    expect(nestedScrollers, 'a nested scroller would capture the sticky header').toEqual([]);
  }
  expect(widths[1]![3]!).toBeGreaterThan(widths[0]![3]! + 200);
  for (const column of [0, 1, 2, 4, 5, 6, 7]) {
    expect(Math.abs(widths[1]![column]! - widths[0]![column]!)).toBeLessThan(2);
  }

  await scroller.evaluate((el) => { el.scrollLeft = el.scrollWidth; });
  await expect(first.getByRole('button', { name: 'Deny', exact: true })).toBeInViewport();
  await scroller.evaluate((el) => { el.scrollLeft = 0; el.scrollTop = 400; });
  await expect.poll(async () => {
    const header = await table.locator('th').first().boundingBox();
    const card = await scroller.boundingBox();
    return Math.abs(header!.y - card!.y);
  }, { message: 'header remains pinned during virtual scrolling' }).toBeLessThan(2);

  // Shorter values should fit a compact desktop without a fixed table
  // minimum forcing otherwise unnecessary horizontal scrolling.
  await page.unroute(/\/api\/v1\/queries(?:\?|$)/);
  await page.route(/\/api\/v1\/queries(?:\?|$)/, (route) => route.fulfill({
    json: { queries: rows.map((r) => ({ ...r, client: { ip: '10.200.200.1' }, node: 'kudurru-01', reply: { type: 'ip', time_ms: 7.8 } })), records_total: rows.length, records_filtered: rows.length },
  }));
  await page.setViewportSize({ width: 1100, height: 900 });
  await page.reload();
  await settle(page);
  await expect(first).toContainText('kudurru-01');
  await expect.poll(() => scroller.evaluate((el) => el.scrollWidth - el.clientWidth), { message: 'compact table fits available width' }).toBeLessThan(2);
  await expect(first.getByRole('button', { name: 'Deny', exact: true })).toBeInViewport();
});
