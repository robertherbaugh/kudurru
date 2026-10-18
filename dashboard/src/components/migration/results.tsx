import { Icon } from '@astryxdesign/core/Icon';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { CheckCircle2, Clock, MinusCircle, XCircle } from 'lucide-react';
import type { MigrationRun } from '../../api/types';

export type MigrationItem = MigrationRun['data']['items'][number] & Record<string, unknown>;
const success = new Set(['created', 'updated', 'unchanged']);

export function migrationCounts(run: MigrationRun) {
  const items = run.data.items;
  const succeeded = items.filter((i) => success.has(i.result)).length;
  const failed = items.filter((i) => i.result === 'error').length;
  const skipped = items.filter((i) => i.result === 'skipped').length;
  const notApplied = items.filter((i) => i.result === 'not_applied').length;
  const pending = items.length - succeeded - failed - skipped - notApplied;
  return { succeeded, failed, skipped, notApplied, pending, selected: items.length - skipped, processed: succeeded + failed + notApplied };
}

export function MigrationItemStatus({ item }: { item: MigrationItem }) {
  const ok = success.has(item.result);
  const error = item.result === 'error';
  const labels: Record<string, string> = { created: 'Created', updated: 'Updated', unchanged: 'Already up to date', error: 'Failed', skipped: 'Not selected', pending: 'Pending', running: 'Importing', not_applied: 'Not applied', unconfirmed: 'Unconfirmed' };
  return <HStack gap={2} vAlign="center"><Icon icon={ok ? CheckCircle2 : error ? XCircle : item.result === 'skipped' ? MinusCircle : Clock} color={ok ? 'success' : error ? 'error' : 'secondary'} /><Text>{labels[item.result] ?? item.result}</Text></HStack>;
}

const columns: TableColumn<MigrationItem>[] = [
  { key: 'result', header: 'Result', renderCell: (i) => <MigrationItemStatus item={i} /> },
  { key: 'kind', header: 'Kind', renderCell: (i) => <Text>{i.kind}</Text> },
  { key: 'name', header: 'Object or settings section', renderCell: (i) => <Text type="code">{i.namespace ? `${i.namespace}/` : ''}{i.name}</Text> },
  { key: 'error', header: 'Details', renderCell: (i) => <VStack gap={1}>
    <Text>{i.error || (i.result === 'skipped' ? 'Not included in this import.' : i.result === 'pending' ? 'Waiting to import.' : i.result === 'running' ? 'Applying the reviewed configuration.' : '—')}</Text>
    {i.errorKey ? <Text type="supporting">Reason: {i.errorKey}</Text> : null}
    {i.hint ? <Text type="supporting">{i.hint}</Text> : null}
    {i.resourceVersion ? <Text type="supporting">Saved version: {i.resourceVersion}</Text> : null}
  </VStack> },
];

export function MigrationResults({ items, label = 'Import results' }: { items: MigrationItem[]; label?: string }) {
  return <Table<MigrationItem> aria-label={label} data={items} columns={columns} idKey={(i) => `${i.kind}/${i.namespace}/${i.name}`} density="compact" verticalAlign="top" />;
}
