// The dashboard (§10 principles 1-3): stat tiles, the 24 h stacked bars,
// the per-client chart, query-type and upstream donuts, and the top
// permitted/blocked domains and top clients. Every number links into the
// query log with the matching filters.
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { qk, useApi, useScopeParams } from '../api/context';
import { useSummary } from '../api/hooks';
import type { ClientTimeseries, Timeseries } from '../api/types';
import { PieChart, type PieItem } from '../charts/PieChart';
import { StackedBars, type StackSeries } from '../charts/StackedBars';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ScopeNote } from '../components/ScopeSelector';
import { StatTiles } from '../components/StatTiles';
import { TopList } from '../components/TopList';
import { useNavigate } from '../lib/router';
import { Badge } from '@astryxdesign/core/Badge';

/** The query-log route for a chart bucket: its start ±5 minutes. */
export function bucketRoute(tsSeconds: number, stepSeconds: number, extra: Record<string, string> = {}): string {
  const p = new URLSearchParams(extra);
  p.set('since', new Date((tsSeconds - 300) * 1000).toISOString());
  p.set('until', new Date((tsSeconds + Math.max(stepSeconds, 0) + 300) * 1000).toISOString());
  return `/queries?${p.toString()}`;
}

/** Splits the 24 h series into the stacked categories (total is their sum). */
export function seriesFor(ts: Timeseries | undefined): { x: number[]; series: StackSeries[] } {
  const pts = ts?.points ?? [];
  const col = (k: 'blocked' | 'cached' | 'forwarded' | 'authoritative' | 'other') => pts.map((p) => p[k]);
  return {
    x: pts.map((p) => Math.floor(new Date(p.ts).getTime() / 1000)),
    series: [
      { key: 'forwarded', label: 'Forwarded', color: 'var(--q-forwarded)', values: col('forwarded') },
      { key: 'cached', label: 'Cached', color: 'var(--q-cached)', values: col('cached') },
      { key: 'authoritative', label: 'Authoritative', color: 'var(--q-authoritative)', values: col('authoritative') },
      { key: 'other', label: 'Other', color: 'var(--q-other)', values: col('other') },
      { key: 'blocked', label: 'Blocked', color: 'var(--q-blocked)', values: col('blocked') },
    ],
  };
}

const CLIENT_COLORS = ['blue', 'teal', 'orange', 'purple', 'pink', 'green', 'cyan', 'indigo', 'brown', 'red'];

/** One series per top client plus "other clients" (the last counts column). */
export function clientSeriesFor(ct: ClientTimeseries | undefined): { x: number[]; series: StackSeries[] } {
  const pts = ct?.points ?? [];
  const clients = ct?.clients ?? [];
  const series: StackSeries[] = clients.map((c, i) => ({
    key: c.key,
    label: c.name ?? c.key,
    color: `var(--color-data-categorical-${CLIENT_COLORS[i % CLIENT_COLORS.length]})`,
    values: pts.map((p) => p.counts[i] ?? 0),
  }));
  if (pts.some((p) => p.counts.length > clients.length)) {
    series.push({ key: '__other', label: 'Other clients', color: 'var(--q-other)', values: pts.map((p) => p.counts[clients.length] ?? 0) });
  }
  return { x: pts.map((p) => Math.floor(new Date(p.ts).getTime() / 1000)), series };
}

export default function DashboardPage() {
  const api = useApi();
  const scope = useScopeParams();
  const navigate = useNavigate();
  const summary = useSummary();
  const ts = useQuery({ queryKey: qk.stats('timeseries', scope, '24h'), queryFn: () => api.stats.timeseries(scope), refetchInterval: 60_000 });
  const clients = useQuery({
    queryKey: qk.stats('clients-ts', scope),
    queryFn: () => api.stats.clientsTimeseries(scope, { n: 8, window: '24h' }),
    refetchInterval: 60_000,
  });
  const topPermitted = useQuery({ queryKey: qk.stats('top', scope, 'domains', false), queryFn: () => api.stats.top(scope, 'domains', { n: 10, blocked: false, window: '24h' }) });
  const topBlocked = useQuery({ queryKey: qk.stats('top', scope, 'domains', true), queryFn: () => api.stats.top(scope, 'domains', { n: 10, blocked: true, window: '24h' }) });
  const topClients = useQuery({ queryKey: qk.stats('top', scope, 'clients', false), queryFn: () => api.stats.top(scope, 'clients', { n: 10, window: '24h' }) });
  const upstreams = useQuery({ queryKey: qk.stats('upstreams', scope), queryFn: () => api.stats.upstreams(scope, '24h') });

  const main = useMemo(() => seriesFor(ts.data), [ts.data]);
  const perClient = useMemo(() => clientSeriesFor(clients.data), [clients.data]);
  const types = useMemo<PieItem[]>(
    () =>
      Object.entries(summary.data?.types ?? {})
        .sort((a, b) => b[1] - a[1])
        .map(([name, value]) => ({ name, value, href: `/queries?qtype=${encodeURIComponent(name)}` })),
    [summary.data],
  );
  const ups = useMemo<PieItem[]>(
    () => (upstreams.data?.upstreams ?? []).map((u) => ({ name: u.addr, value: u.total, href: `/queries?upstream=${encodeURIComponent(u.addr)}` })),
    [upstreams.data],
  );

  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Dashboard</h1>
        <ScopeNote scope={summary.data} />
      </div>
      {summary.error ? <ErrorBanner error={summary.error} /> : null}
      <StatTiles summary={summary.data} />

      <section className="k-panel" aria-labelledby="chart-total">
        <div className="k-panel-title">
          <h2 id="chart-total">Queries over the last 24 hours</h2>
          {ts.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
          <span className="k-muted k-small">Click a bar to open the log around it</span>
        </div>
        {ts.error ? <ErrorBanner error={ts.error} /> : null}
        {ts.data ? (
          <StackedBars
            label="Stacked bars of forwarded, cached, authoritative, other and blocked queries per bucket over 24 hours"
            x={main.x}
            series={main.series}
            step={ts.data.step}
            onBucketClick={(t) => navigate(bucketRoute(t, ts.data?.step ?? 0), { replace: false })}
            bucketHref={(t) => bucketRoute(t, ts.data?.step ?? 0)}
          />
        ) : (
          !ts.error && <Loading />
        )}
      </section>

      <section className="k-panel" aria-labelledby="chart-clients">
        <div className="k-panel-title">
          <h2 id="chart-clients">Queries per client</h2>
        </div>
        {clients.error ? <ErrorBanner error={clients.error} /> : null}
        {clients.data ? (
          <StackedBars
            label="Stacked bars of queries per client over 24 hours, with other clients grouped"
            x={perClient.x}
            series={perClient.series}
            step={clients.data.step}
            height={200}
            onBucketClick={(t) => navigate(bucketRoute(t, clients.data?.step ?? 0))}
            bucketHref={(t) => bucketRoute(t, clients.data?.step ?? 0)}
          />
        ) : (
          !clients.error && <Loading />
        )}
      </section>

      <div className="k-grid k-grid-2">
        <section className="k-panel" aria-labelledby="pie-types">
          <div className="k-panel-title">
            <h2 id="pie-types">Query types</h2>
          </div>
          {summary.data ? <PieChart label="Share of query types" items={types} onSliceClick={(i) => i.href && navigate(i.href)} /> : <Loading />}
        </section>
        <section className="k-panel" aria-labelledby="pie-upstreams">
          <div className="k-panel-title">
            <h2 id="pie-upstreams">Upstreams</h2>
            {upstreams.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
          </div>
          {upstreams.error ? <ErrorBanner error={upstreams.error} /> : null}
          {upstreams.data ? (
            <PieChart label="Share of queries per upstream" items={ups} onSliceClick={(i) => i.href && navigate(i.href)} />
          ) : (
            !upstreams.error && <Loading />
          )}
        </section>
      </div>

      <div className="k-grid k-grid-3">
        <TopList title="Top permitted domains" dim="domains" data={topPermitted.data} isLoading={topPermitted.isLoading} error={topPermitted.error} />
        <TopList title="Top blocked domains" dim="domains" blocked data={topBlocked.data} isLoading={topBlocked.isLoading} error={topBlocked.error} />
        <TopList title="Top clients" dim="clients" data={topClients.data} isLoading={topClients.isLoading} error={topClients.error} />
      </div>
    </div>
  );
}
