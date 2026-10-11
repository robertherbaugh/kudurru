// Long-term views (§10 principle 12): a date range over the rollups, the
// stacked series at step=auto, totals summed from it, and top lists for
// the range, each with the "approximate" badge when the server merged
// top-N rollups lossily. Drill-downs carry the range into the log.
import { Badge } from '@astryxdesign/core/Badge';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { useQuery } from '@tanstack/react-query';
import { useId, useMemo } from 'react';
import { qk, useApi, useScopeParams } from '../api/context';
import type { TopDim } from '../api/types';
import { StackedBars } from '../charts/StackedBars';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ScopeNote } from '../components/ScopeSelector';
import { TopList } from '../components/TopList';
import { formatCount, formatExact, formatPercent } from '../lib/format';
import { useNavigate, useSearchParams } from '../lib/router';
import { bucketRoute, seriesFor } from './DashboardPage';
import { fromLocalInput, toLocalInput } from './QueryLogPage';

export const PRESETS: { value: string; label: string; days: number }[] = [
  { value: '7d', label: '7 days', days: 7 },
  { value: '30d', label: '30 days', days: 30 },
  { value: '90d', label: '90 days', days: 90 },
  { value: '365d', label: '1 year', days: 365 },
];

/** The [from, to] range from the URL: explicit from/to win over ?range=. */
export function rangeFrom(sp: URLSearchParams, now = Date.now()): { from: string; to: string; preset: string } {
  const from = sp.get('from');
  const to = sp.get('to');
  if (from && to) return { from, to, preset: 'custom' };
  const p = PRESETS.find((x) => x.value === sp.get('range')) ?? PRESETS[0];
  const days = p?.days ?? 7;
  return { from: new Date(now - days * 86_400_000).toISOString(), to: new Date(now).toISOString(), preset: p?.value ?? '7d' };
}

const DIMS: { dim: TopDim; title: string; blocked?: boolean }[] = [
  { dim: 'domains', title: 'Top permitted domains' },
  { dim: 'domains', title: 'Top blocked domains', blocked: true },
  { dim: 'clients', title: 'Top clients' },
  { dim: 'upstreams', title: 'Top upstreams' },
  { dim: 'lists', title: 'Lists that blocked most', blocked: true },
  { dim: 'zones', title: 'Top local zones' },
];

function TopFor({ dim, title, blocked, from, to }: { dim: TopDim; title: string; blocked?: boolean; from: string; to: string }) {
  const api = useApi();
  const scope = useScopeParams();
  const q = useQuery({
    queryKey: qk.stats('top-range', scope, dim, !!blocked, from, to),
    queryFn: () => api.stats.top(scope, dim, { n: 10, blocked: !!blocked, from, to }),
  });
  return <TopList title={title} dim={dim} blocked={blocked} data={q.data} isLoading={q.isLoading} error={q.error} extra={{ since: from, until: to }} />;
}

export default function LongTermPage() {
  const api = useApi();
  const scope = useScopeParams();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  // Round "now" to the minute so the query key is stable across renders.
  const now = Math.floor(Date.now() / 60_000) * 60_000;
  const { from, to, preset } = rangeFrom(sp, now);
  const fromId = useId();
  const toId = useId();
  const ts = useQuery({ queryKey: qk.stats('timeseries-range', scope, from, to), queryFn: () => api.stats.timeseries(scope, { from, to }) });
  const main = useMemo(() => seriesFor(ts.data), [ts.data]);
  const totals = useMemo(() => {
    const pts = ts.data?.points ?? [];
    const total = pts.reduce((a, p) => a + p.total, 0);
    const blocked = pts.reduce((a, p) => a + p.blocked, 0);
    const cached = pts.reduce((a, p) => a + p.cached, 0);
    return { total, blocked, cached, pct: total ? (blocked / total) * 100 : 0 };
  }, [ts.data]);

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Long-term</h1>
          <ScopeNote scope={ts.data} />
        </div>
        <div className="k-actions">
          <SegmentedControl label="Range" value={preset === 'custom' ? '' : preset} onChange={(v) => setParams({ range: v, from: null, to: null })}>
            {PRESETS.map((p) => (
              <SegmentedControlItem key={p.value} value={p.value} label={p.label} />
            ))}
          </SegmentedControl>
          <label className="k-label" htmlFor={fromId}>
            From
            <input id={fromId} className="k-native-input" type="datetime-local" value={toLocalInput(from)} onChange={(e) => setParams({ from: fromLocalInput(e.target.value) || null, to })} />
          </label>
          <label className="k-label" htmlFor={toId}>
            To
            <input id={toId} className="k-native-input" type="datetime-local" value={toLocalInput(to)} onChange={(e) => setParams({ to: fromLocalInput(e.target.value) || null, from })} />
          </label>
        </div>
      </div>

      <div className="k-grid k-grid-tiles" role="list" aria-label="Totals for the range">
        <div className="k-tile" data-tone="total" role="listitem">
          <span className="k-tile-label">Total queries</span>
          <span className="k-tile-value" title={formatExact(totals.total)}>
            {ts.data ? formatCount(totals.total) : '…'}
          </span>
        </div>
        <div className="k-tile" data-tone="blocked" role="listitem">
          <span className="k-tile-label">Blocked</span>
          <span className="k-tile-value">{ts.data ? formatCount(totals.blocked) : '…'}</span>
        </div>
        <div className="k-tile" data-tone="percent" role="listitem">
          <span className="k-tile-label">Percent blocked</span>
          <span className="k-tile-value">{ts.data ? formatPercent(totals.pct) : '…'}</span>
        </div>
        <div className="k-tile" data-tone="lists" role="listitem">
          <span className="k-tile-label">Answered from cache</span>
          <span className="k-tile-value">{ts.data ? formatCount(totals.cached) : '…'}</span>
        </div>
      </div>

      <section className="k-panel" aria-labelledby="lt-chart">
        <div className="k-panel-title">
          <h2 id="lt-chart">Queries in the range</h2>
          {ts.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
          {ts.data ? <span className="k-muted k-small">{Math.round(ts.data.step / 60)} min buckets</span> : null}
        </div>
        {ts.error ? <ErrorBanner error={ts.error} /> : null}
        {ts.data ? (
          <StackedBars
            label="Stacked bars of queries per bucket over the selected range"
            x={main.x}
            series={main.series}
            step={ts.data.step}
            onBucketClick={(t) => navigate(bucketRoute(t, ts.data?.step ?? 0))}
            bucketHref={(t) => bucketRoute(t, ts.data?.step ?? 0)}
          />
        ) : (
          !ts.error && <Loading />
        )}
      </section>

      <div className="k-grid k-grid-3">
        {DIMS.map((d) => (
          <TopFor key={`${d.dim}-${d.blocked ? 'b' : 'p'}`} {...d} from={from} to={to} />
        ))}
      </div>
    </div>
  );
}
