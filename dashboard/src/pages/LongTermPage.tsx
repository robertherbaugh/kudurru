// Long-term views (§10 principle 12): a date range over the rollups, the
// stacked series at step=auto, totals summed from it, and top lists for
// the range, each with the "approximate" badge when the server merged
// top-N rollups lossily. Drill-downs carry the range into the log.
import { Badge } from '@astryxdesign/core/Badge';
import { Card } from '@astryxdesign/core/Card';
import { DateTimeInput, type ISODateTimeString } from '@astryxdesign/core/DateTimeInput';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { qk, useApi, useScopeParams } from '../api/context';
import { useFeature } from '../api/hooks';
import type { TopDim } from '../api/types';
import { StackedBars } from '../charts/StackedBars';
import { ErrorBanner, Loading } from '../components/Feedback';
import { FeatureLocked } from '../components/FeatureLocked';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { ScopeSelector } from '../components/ScopeSelector';
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

/** Long-term views are licensed (feature "longterm"); locked, the page asks for nothing. */
export default function LongTermPage() {
  const lt = useFeature('longterm');
  if (!lt.ready) {
    return (
      <Page>
        <PageHeader title="Long-term" />
        <Loading />
      </Page>
    );
  }
  if (!lt.allowed) {
    return (
      <Page>
        <PageHeader title="Long-term" description="Statistics for periods older than 24 hours." />
        <FeatureLocked feature="longterm" what="Long-term statistics" />
      </Page>
    );
  }
  return <LongTermView />;
}

function LongTermView() {
  const api = useApi();
  const scope = useScopeParams();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  // Round "now" to the minute so the query key is stable across renders.
  const now = Math.floor(Date.now() / 60_000) * 60_000;
  const { from, to, preset } = rangeFrom(sp, now);
  const ts = useQuery({ queryKey: qk.stats('timeseries-range', scope, from, to), queryFn: () => api.stats.timeseries(scope, { from, to }) });
  const main = useMemo(() => seriesFor(ts.data), [ts.data]);
  const totals = useMemo(() => {
    const pts = ts.data?.points ?? [];
    const total = pts.reduce((a, p) => a + p.total, 0);
    const blocked = pts.reduce((a, p) => a + p.blocked, 0);
    const cached = pts.reduce((a, p) => a + p.cached, 0);
    return { total, blocked, cached, pct: total ? (blocked / total) * 100 : 0 };
  }, [ts.data]);

  const tiles: { tone: string; label: string; value: number; exact?: boolean }[] = [
    { tone: 'total', label: 'Total queries', value: totals.total, exact: true },
    { tone: 'blocked', label: 'Blocked', value: totals.blocked },
    { tone: 'percent', label: 'Percent blocked', value: totals.pct },
    { tone: 'lists', label: 'Answered from cache', value: totals.cached },
  ];

  return (
    <Page>
      <VStack gap={2}>
        <PageHeader title="Long-term" />
        <HStack gap={2} vAlign="end" wrap="wrap" role="group" aria-label="Long-term controls">
          <ScopeSelector allLabel="All" response={ts.data} />
          <SegmentedControl size="sm" label="Range" value={preset === 'custom' ? '' : preset} onChange={(v) => setParams({ range: v, from: null, to: null })}>
            {PRESETS.map((p) => (
              <SegmentedControlItem key={p.value} value={p.value} label={p.label} />
            ))}
          </SegmentedControl>
          <StackItem size="fill" className="k-range-bound">
            <DateTimeInput
              label="From"
              size="sm"
              width="100%"
              className="k-compact-datetime"
              value={(toLocalInput(from) || undefined) as ISODateTimeString | undefined}
              onChange={(v) => setParams({ from: fromLocalInput(v ?? '') || null, to })}
            />
          </StackItem>
          <StackItem size="fill" className="k-range-bound">
            <DateTimeInput
              label="To"
              size="sm"
              width="100%"
              className="k-compact-datetime"
              value={(toLocalInput(to) || undefined) as ISODateTimeString | undefined}
              onChange={(v) => setParams({ to: fromLocalInput(v ?? '') || null, from })}
            />
          </StackItem>
        </HStack>
      </VStack>

      <Grid columns={{ minWidth: 200, repeat: 'fit' }} gap={4} role="list" aria-label="Totals for the range">
        {tiles.map((t) => (
          // The coloured start edge names the tile's query class (styles.css .k-tile, as in StatTiles).
          <Card key={t.tone} className="k-tile" data-tone={t.tone} role="listitem">
            <VStack gap={1}>
              <Text type="supporting">{t.label}</Text>
              <Tooltip content={formatExact(t.value)} isEnabled={!!t.exact && !!ts.data} hasHoverIndication={false}>
                <Text type="display-3" weight="semibold" hasTabularNumbers>
                  {ts.data ? (t.tone === 'percent' ? formatPercent(t.value) : formatCount(t.value)) : '…'}
                </Text>
              </Tooltip>
            </VStack>
          </Card>
        ))}
      </Grid>

      <Panel
        title="Queries in the range"
        headingId="lt-chart"
        actions={
          <>
            {ts.data?.approximate ? <Badge variant="warning" label="approximate" /> : null}
            {ts.data ? (
              <Text type="supporting">
                {ts.data.source === 'central' ? 'Central database · ' : ''}
                {Math.round(ts.data.step / 60)} min buckets
              </Text>
            ) : null}
          </>
        }
      >
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
      </Panel>

      <Grid columns={{ minWidth: 320, repeat: 'fit' }} gap={4}>
        {DIMS.map((d) => (
          <TopFor key={`${d.dim}-${d.blocked ? 'b' : 'p'}`} {...d} from={from} to={to} />
        ))}
      </Grid>
    </Page>
  );
}
