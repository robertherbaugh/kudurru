// The additional query-log sinks of the node that serves the dashboard
// (querylog.sinks, D371; dnstap D372): GET /querylog/sinks reports each
// sink's queue, sent, dropped and failed-attempt counters and its last
// success. Every node runs its own sinks, so the panel names the node that
// answered. The route never carries a credential, a URL, an error text or a
// query, and this panel renders only the fields named here: whatever else a
// server might add is not shown.
import { Badge } from '@astryxdesign/core/Badge';
import { Code } from '@astryxdesign/core/Code';
import { HStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useQuery } from '@tanstack/react-query';
import { useApi, useCan } from '../api/context';
import type { QuerylogSink } from '../api/types';
import { formatDateTime, formatExact, formatRelative } from '../lib/format';
import { ErrorBanner } from './Feedback';
import { Panel } from './layout/Page';

export const querylogSinksKey = ['querylog', 'sinks'] as const;

const TYPE_LABELS: Record<string, string> = { slog: 'Log lines', clickhouse: 'ClickHouse', loki: 'Loki', dnstap: 'dnstap' };

/** The fields the panel shows, copied one by one so nothing else of the answer reaches the page. */
export function sinkRow(s: QuerylogSink): QuerylogSink {
  return {
    name: s.name,
    type: s.type,
    raw: s.raw,
    queued: s.queued,
    queue_size: s.queue_size,
    sent: s.sent,
    dropped: s.dropped,
    errors: s.errors,
    ...(s.last_success ? { last_success: s.last_success } : {}),
  };
}

type Row = QuerylogSink & Record<string, unknown>;

const columns: TableColumn<Row>[] = [
  {
    key: 'name',
    header: 'Sink',
    width: proportional(2, { minWidth: 180 }),
    renderCell: (s) => (
      <HStack gap={1} vAlign="center" wrap="wrap">
        <Text type="code" size="sm">
          {s.name}
        </Text>
        <Text type="supporting">{TYPE_LABELS[s.type] ?? s.type}</Text>
        {s.raw ? (
          <Tooltip content="Receives queries as they arrived, before the privacy transform (querylog.raw)">
            <Badge variant="warning" label="raw" />
          </Tooltip>
        ) : null}
      </HStack>
    ),
  },
  {
    key: 'queued',
    header: 'Queue',
    align: 'end',
    width: proportional(1, { minWidth: 110 }),
    renderCell: (s) => (
      <Text hasTabularNumbers>
        {formatExact(s.queued)} / {formatExact(s.queue_size)}
      </Text>
    ),
  },
  { key: 'sent', header: 'Sent', align: 'end', width: proportional(1, { minWidth: 90 }), renderCell: (s) => <Text hasTabularNumbers>{formatExact(s.sent)}</Text> },
  {
    key: 'dropped',
    header: 'Dropped',
    align: 'end',
    width: proportional(1, { minWidth: 90 }),
    renderCell: (s) => (
      <Text hasTabularNumbers>
        {formatExact(s.dropped)}
      </Text>
    ),
  },
  {
    key: 'errors',
    header: 'Failed attempts',
    align: 'end',
    width: proportional(1, { minWidth: 120 }),
    renderCell: (s) => (
      <Text hasTabularNumbers>
        {formatExact(s.errors)}
      </Text>
    ),
  },
  {
    key: 'last_success',
    header: 'Last success',
    width: proportional(1.2, { minWidth: 120 }),
    renderCell: (s) =>
      s.last_success ? (
        <Tooltip content={formatDateTime(s.last_success)}>
          <Text>{formatRelative(s.last_success)}</Text>
        </Tooltip>
      ) : (
        <Text color="secondary">never</Text>
      ),
  },
];

/** The sink status of the answering node (needs querylog.read). */
export function QuerylogSinksPanel() {
  const api = useApi();
  const can = useCan();
  const readable = can('querylog', 'read');
  const q = useQuery({ queryKey: querylogSinksKey, queryFn: () => api.queries.sinks(), enabled: readable, refetchInterval: 15_000 });
  if (!readable) return null;
  const rows = (q.data?.sinks ?? []).map(sinkRow) as Row[];
  const failing = rows.some((s) => s.errors > 0 || s.dropped > 0);
  return (
    <Panel
      title="Query-log sinks"
      headingId="querylog-sinks-title"
      description={
        <>
          The destinations of <Code size="inherit">querylog.sinks</Code> as {q.data?.node ? <Code size="inherit">{q.data.node}</Code> : 'this node'} runs them, beside the local query log. Every node runs its own sinks; a slow or failing sink drops
          its own queue and never holds up DNS or the local log.
          {failing ? ' The reason of a failed attempt is in this node’s log and the kudurru_sink_errors_total metric; the dashboard never shows sink credentials or addresses.' : ''}
        </>
      }
    >
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.data ? (
        <Table<Row> aria-label="Query-log sinks" density="compact" idKey="name" data={rows} columns={columns} emptyState={<Text color="secondary">No additional sinks are configured on this node.</Text>} />
      ) : null}
    </Panel>
  );
}
