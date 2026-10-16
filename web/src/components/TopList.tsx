// Top-N tables (domains, clients, upstreams, lists, zones) where every row
// drills down into the query log (§10 principle 3), with the "approximate"
// badge when the server merged lossy top-N rollups (principle 12).
import { Badge } from '@astryxdesign/core/Badge';
import { Link } from '@astryxdesign/core/Link';
import { ProgressBar } from '@astryxdesign/core/ProgressBar';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import type { TopDim, TopItem, TopResult } from '../api/types';
import { formatExact, formatPercent } from '../lib/format';
import { ErrorBanner, Loading } from './Feedback';
import { Panel } from './layout/Page';

/** The query-log route for one top-list row. */
export function drillHref(dim: TopDim, key: string, blocked: boolean, extra: Record<string, string> = {}): string {
  const p = new URLSearchParams();
  const field = dim === 'domains' ? 'domain' : dim === 'clients' ? 'client' : dim === 'upstreams' ? 'upstream' : dim === 'lists' ? 'list' : 'zone';
  p.set(field, key);
  if (blocked) p.set('status_class', 'blocked');
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return `/queries?${p.toString()}`;
}

export interface TopListProps {
  title: string;
  dim: TopDim;
  blocked?: boolean;
  data: TopResult | undefined;
  isLoading?: boolean;
  error?: unknown;
  /** Extra filters carried into the drill-down (since/until for long-term views). */
  extra?: Record<string, string>;
}

export function TopList({ title, dim, blocked = false, data, isLoading, error, extra }: TopListProps) {
  const max = data?.items.reduce((m, i) => Math.max(m, i.count), 0) ?? 0;
  const headingId = `top-${dim}-${blocked ? 'b' : 'p'}-${title.replace(/\W+/g, '-')}`;
  const columns: TableColumn<Row>[] = [
    {
      key: 'key',
      header: dim === 'domains' ? 'Domain' : dim === 'clients' ? 'Client' : dim === 'upstreams' ? 'Upstream' : dim === 'lists' ? 'List' : 'Zone',
      width: proportional(3, { minWidth: 140 }),
      renderCell: (i) => (
        // break-all: the link is inline-flex, so a long domain could not wrap and was cut off.
        <Text maxLines={2} wordBreak="break-all">
          <Link href={drillHref(dim, i.key, blocked, extra)} color="inherit" hasUnderline tooltip="Show these queries in the log">
            {i.name ? `${i.name}` : i.key}
          </Link>
          {i.name && i.name !== i.key ? <Text type="supporting"> {i.key}</Text> : null}
        </Text>
      ),
    },
    {
      key: 'count',
      header: blocked ? 'Blocked' : 'Queries',
      align: 'end',
      width: pixel(96),
      renderCell: (i) => <Text hasTabularNumbers>{formatExact(i.count)}</Text>,
    },
    {
      key: 'percent',
      header: 'Share',
      width: proportional(2, { minWidth: 80 }),
      renderCell: (i) => (
        <ProgressBar label={`${formatPercent(i.percent)} of all ${blocked ? 'blocked queries' : 'queries'}`} isLabelHidden value={i.count} max={max || 1} variant={blocked ? 'error' : 'success'} />
      ),
    },
  ];
  return (
    <Panel
      title={title}
      headingId={headingId}
      actions={
        data?.approximate ? (
          <Tooltip content="Merged from top-N rollups; ranks beyond the cutoff may be missing">
            <Badge variant="warning" label="approximate" />
          </Tooltip>
        ) : null
      }
    >
      {error ? <ErrorBanner error={error} /> : null}
      {isLoading && !data ? <Loading /> : null}
      {data && data.items.length === 0 ? <Text color="secondary">No queries in this window.</Text> : null}
      {data && data.items.length > 0 ? <Table<Row> density="compact" hasHover aria-label={title} idKey="key" data={data.items as Row[]} columns={columns} /> : null}
    </Panel>
  );
}

type Row = TopItem & Record<string, unknown>;
