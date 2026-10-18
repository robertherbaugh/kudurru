import { describe, expect, it } from 'vitest';
import type { ClientTimeseries, UpstreamItem } from '../api/types';
import { foldPieItems, type PieItem } from '../charts/PieChart';
import { clientSeriesFor, upstreamItemsFor } from './DashboardPage';

describe('per-client chart API data', () => {
  it('keeps distinct addresses and labels the real API aggregate as Other clients', () => {
    const data: ClientTimeseries = {
      scope: 'cluster', nodes: [], partial: false,
      from: '2026-10-14T00:00:00Z', until: '2026-10-14T00:05:00Z', step: 300,
      clients: [{ key: '192.0.2.10' }, { key: '192.0.2.11' }, { key: 'other' }],
      points: [{ ts: '2026-10-14T00:00:00Z', counts: [4, 3, 2] }],
      approximate: false,
    };
    const chart = clientSeriesFor(data);
    expect(chart.series.map((s) => s.label)).toEqual(['192.0.2.10', '192.0.2.11', 'Other clients']);
    expect(chart.series.map((s) => s.values)).toEqual([[4], [3], [2]]);
    expect(chart.series.at(-1)).toMatchObject({ color: 'var(--q-other)' });
  });
});

const upstream = (addr: string, total: number): UpstreamItem => ({ addr, total, errors: 0, timeouts: 0, percent: 0, avg_ms: 0, p95_ms: 0 });

describe('upstream donut', () => {
  it('folds the answers of local zones into one slice that opens the authoritative log', () => {
    const items = upstreamItemsFor([
      upstream('default/1.1.1.1:53', 50),
      upstream('cache', 30),
      upstream('zone:a.example', 4),
      upstream('zone:b.example', 4),
      upstream('zone:c.example', 22),
    ]);
    expect(items.map((i) => [i.name, i.value])).toEqual([
      ['default/1.1.1.1:53', 50],
      ['cache', 30],
      ['Local zones (3)', 30],
    ]);
    expect(items.at(-1)?.href).toBe('/queries?status=authoritative,local_record');
  });

  it('keeps a single zone as its own slice', () => {
    expect(upstreamItemsFor([upstream('zone:home.arpa', 4), upstream('cache', 9)])).toEqual([
      { name: 'cache', value: 9, href: '/queries?upstream=cache' },
      { name: 'zone:home.arpa', value: 4, href: '/queries?upstream=zone%3Ahome.arpa' },
    ]);
  });

  it('copes with no traffic', () => {
    expect(upstreamItemsFor(null)).toEqual([]);
  });
});

describe('foldPieItems', () => {
  const items: PieItem[] = Array.from({ length: 12 }, (_, i) => ({ name: `u${i}`, value: 100 - i }));

  it('leaves a short list alone', () => {
    expect(foldPieItems(items.slice(0, 10))).toEqual(items.slice(0, 10));
  });

  it('keeps the largest and folds the rest into Other', () => {
    const folded = foldPieItems([...items].reverse(), 4);
    expect(folded.map((i) => [i.name, i.value])).toEqual([
      ['u0', 100],
      ['u1', 99],
      ['u2', 98],
      ['Other (9)', items.slice(3).reduce((a, b) => a + b.value, 0)],
    ]);
    expect(folded.at(-1)?.href).toBeUndefined();
  });
});
