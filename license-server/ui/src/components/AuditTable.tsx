// The audit trail as a table (GET /api/v1/audit): time (UTC), actor,
// action, target and a one-line summary of the details, newest first, with
// "Load older events" following the keyset cursor. The Audit page shows it
// with filters; license and user pages show the events about their object.
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useMemo } from 'react';
import type { AuditEvent, AuditQuery, JsonValue } from '@shared/api';
import { useCan } from '../api/context';
import { useAuditPages } from '../api/hooks';
import { utcDateTime } from '../lib/dates';
import { TARGET_LABEL, targetHref } from '../lib/words';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Panel } from '../ui/components/layout/Page';

type Row = AuditEvent & Record<string, unknown>;

function valueText(v: JsonValue): string {
  if (v === null) return 'none';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  return JSON.stringify(v);
}

/** One value, clipped: long unbroken tokens (an IdP subject, a hash) would otherwise widen the column past the panel. */
function clip(text: string, max = 40): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

/** The details of an event on one line: "key: value · key: value", clipped. */
export function detailsSummary(details: AuditEvent['details'], max = 160): string {
  const s = Object.entries(details)
    .map(([k, v]) => `${k}: ${clip(valueText(v))}`)
    .join(' · ');
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

const columns: TableColumn<Row>[] = [
  { key: 'at', header: 'Time (UTC)', width: pixel(180), renderCell: (e) => utcDateTime(e.at) },
  {
    key: 'actor',
    header: 'Actor',
    width: proportional(2, { minWidth: 150 }),
    renderCell: (e) => (e.actor.id ? <Link href={`/users/${encodeURIComponent(e.actor.id)}`}>{e.actor.label}</Link> : e.actor.label),
  },
  { key: 'action', header: 'Action', width: proportional(2, { minWidth: 200 }), renderCell: (e) => <Text type="code">{e.action}</Text> },
  {
    key: 'target',
    header: 'Target',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (e) => {
      const href = targetHref(e.targetType, e.targetId);
      return (
        <VStack gap={0.5}>
          <Text type="supporting">{TARGET_LABEL[e.targetType]}</Text>
          {href ? <Link href={href}>{e.targetId}</Link> : <Text>{e.targetId}</Text>}
        </VStack>
      );
    },
  },
  { key: 'details', header: 'Details', width: proportional(3, { minWidth: 160 }), renderCell: (e) => <Text type="supporting">{detailsSummary(e.details)}</Text> },
];

export function AuditTable({ query, empty = 'No audit events.' }: { query: Omit<AuditQuery, 'before'>; empty?: string }) {
  const q = useAuditPages(query);
  const events = useMemo<Row[]>(() => (q.data?.pages.flatMap((p) => p.items) ?? []) as Row[], [q.data]);
  if (q.error) return <ErrorBanner error={q.error} />;
  if (q.isPending) return <Loading />;
  return (
    <VStack gap={2}>
      <Table<Row> aria-label="Audit events" data={events} columns={columns} idKey="id" density="compact" verticalAlign="top" emptyState={<Text color="secondary">{empty}</Text>} />
      {q.hasNextPage ? (
        <HStack gap={2}>
          <Button label="Load older events" variant="secondary" size="sm" isLoading={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </HStack>
      ) : null}
    </VStack>
  );
}

/** The recent events about one object, for its page (audit.read only). */
export function RecentAudit({ targetType, targetId }: { targetType: AuditEvent['targetType']; targetId: string }) {
  const can = useCan();
  if (!can('audit.read')) return null;
  return (
    <Panel title="Recent activity" actions={<Link href={`/audit?targetType=${targetType}&targetId=${encodeURIComponent(targetId)}`}>All events</Link>}>
      <AuditTable query={{ targetType, targetId, limit: 10 }} empty="Nothing recorded yet." />
    </Panel>
  );
}
