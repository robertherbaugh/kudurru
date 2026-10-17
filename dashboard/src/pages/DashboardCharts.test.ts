import { describe, expect, it } from 'vitest';
import type { ClientTimeseries } from '../api/types';
import { clientSeriesFor } from './DashboardPage';

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
