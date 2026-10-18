// The dashboard (§10 principles 1-3): stat tiles, the 24 h stacked bars,
// the per-client chart, query-type and upstream donuts, and the top
// permitted/blocked domains and top clients. Every number links into the
// query log with the matching filters.
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { qk, useApi, useScopeParams } from '../api/context';
import { useSummary } from '../api/hooks';
import type { ClientTimeseries, Timeseries, UpstreamItem } from '../api/types';
import { PieChart, type PieItem } from '../charts/PieChart';
import { StackedBars, type StackSeries } from '../charts/StackedBars';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ScopeNote } from '../components/ScopeSelector';
import { StatTiles } from '../components/StatTiles';
import { TopList } from '../components/TopList';
import { UnblockNotice } from '../components/UnblockBadge';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { useNavigate } from '../lib/router';
import { Badge } from '@astryxdesign/core/Badge';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { Text } from '@astryxdesign/core/Text';

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

/** One series per top client plus the API's "other" aggregate. */
export function clientSeriesFor(ct: ClientTimeseries | undefined): { x: number[]; series: StackSeries[] } {
  const pts = ct?.points ?? [];
  const clients = ct?.clients ?? [];
  const series: StackSeries[] = clients.map((c, i) => ({
    key: c.key,
    label: c.key === 'other' ? 'Other clients' : (c.name ?? c.key),
    color: c.key === 'other' ? 'var(--q-other)' : `var(--color-data-categorical-${CLIENT_COLORS[i % CLIENT_COLORS.length]})`,
    values: pts.map((p) => p.counts[i] ?? 0),
  }));
  if (pts.some((p) => p.counts.length > clients.length)) {
    series.push({ key: '__other', label: 'Other clients', color: 'var(--q-other)', values: pts.map((p) => p.counts[clients.length] ?? 0) });
  }
  return { x: pts.map((p) => Math.floor(new Date(p.ts).getTime() / 1000)), series };
}

/** The prefix GET /stats/upstreams gives answers from a local zone ("zone:<name>"). */
const ZONE_UPSTREAM = 'zone:';

/**
 * The upstream donut's slices, largest first. Answers from local zones are
 * one "Local zones (n)" slice that opens the log of authoritative answers:
 * every zone is its own row in the API, so a server with many zones would
 * otherwise list each of them.
 */
export function upstreamItemsFor(rows: UpstreamItem[] | null | undefined): PieItem[] {
  const item = (u: UpstreamItem): PieItem => ({ name: u.addr, value: u.total, href: `/queries?upstream=${encodeURIComponent(u.addr)}` });
  const zones = (rows ?? []).filter((u) => u.addr.startsWith(ZONE_UPSTREAM));
  const items = (rows ?? []).filter((u) => !u.addr.startsWith(ZONE_UPSTREAM)).map(item);
  if (zones.length > 1) {
    items.push({
      name: `Local zones (${zones.length})`,
      value: zones.reduce((a, b) => a + b.total, 0),
      href: '/queries?status=authoritative,local_record',
    });
  } else {
    items.push(...zones.map(item));
  }
  return items.sort((a, b) => b.value - a.value);
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
  const ups = useMemo(() => upstreamItemsFor(upstreams.data?.upstreams), [upstreams.data]);

  return (
    <Page>
      <PageHeader title="Dashboard" actions={<ScopeNote scope={summary.data} />} />
      {summary.error ? <ErrorBanner error={summary.error} /> : null}
      <UnblockNotice />
      <StatTiles summary={summary.data} />

      <Panel
        title="Queries over the last 24 hours"
        headingId="chart-total"
        actions={
          <>
            {ts.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
            <Text type="supporting">Click a bar to open the log around it</Text>
          </>
        }
      >
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
      </Panel>

      <Panel title="Queries per client" headingId="chart-clients">
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
      </Panel>

      <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
        <Panel title="Query types" headingId="pie-types">
          {summary.data ? <PieChart label="Share of query types" items={types} onSliceClick={(i) => i.href && navigate(i.href)} /> : <Loading />}
        </Panel>
        <Panel
          title="Upstreams"
          headingId="pie-upstreams"
          actions={
            <>
              {upstreams.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
              {upstreams.data?.servers?.length ? (
                <Text type="supporting" data-testid="upstream-health">
                  {upstreams.data.servers.filter((s) => s.healthy).length} of {upstreams.data.servers.length} servers healthy
                </Text>
              ) : null}
              <Link href="/upstreams" size="sm">
                Health and configuration
              </Link>
            </>
          }
        >
          {upstreams.error ? <ErrorBanner error={upstreams.error} /> : null}
          {upstreams.data ? (
            <PieChart label="Share of queries per upstream" items={ups} onSliceClick={(i) => i.href && navigate(i.href)} />
          ) : (
            !upstreams.error && <Loading />
          )}
        </Panel>
      </Grid>

      <Grid columns={{ minWidth: 320, repeat: 'fit' }} gap={4}>
        <TopList title="Top permitted domains" dim="domains" data={topPermitted.data} isLoading={topPermitted.isLoading} error={topPermitted.error} />
        <TopList title="Top blocked domains" dim="domains" blocked data={topBlocked.data} isLoading={topBlocked.isLoading} error={topBlocked.error} />
        <TopList title="Top clients" dim="clients" data={topClients.data} isLoading={topClients.isLoading} error={topClients.error} />
      </Grid>
    </Page>
  );
}
