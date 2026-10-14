// Zones (§10 principle 11): type, serial, record count, Ready and the
// manager of every zone and transfer/NOTIFY status. New zones are created
// with the wizard (a modal, ?new=1); a BIND file is imported in a dialog.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { ZoneSummary } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { Page, PageHeader } from '../components/layout/Page';
import { ManagedBadge } from '../components/ManagedBadge';
import { SecondaryAvailability } from '../components/SecondaryRuntime';
import { StubAvailability } from '../components/StubRuntime';
import { ZoneWizardDialog } from '../components/wizards/ZoneWizard';
import { ZoneDeleteDialog } from '../components/ZoneDeleteDialog';
import { ZoneImportDialog } from '../components/ZoneImportDialog';
import { formatExact, formatRelative } from '../lib/format';
import { useNewParam, useSearchParams } from '../lib/router';

/** Text in the colour of an outcome (the query-status palette). */
const outcome = (ok: boolean) => ({ color: ok ? 'var(--q-permitted)' : 'var(--q-blocked)' });

export function TransferCell({ z }: { z: ZoneSummary }) {
  if (z.type === 'secondary') {
    return <Text size="sm">{z.secondaryRuntime?.primary ? `AXFR/IXFR from ${z.secondaryRuntime.primary}` : 'AXFR/IXFR'}</Text>;
  }
  if (z.type === 'stub') return <Text color="secondary">No zone transfers</Text>;
  const t = z.transfer;
  if (!t) return <Text color="secondary">–</Text>;
  return (
    <VStack gap={0.5}>
      <Text size="sm">
        AXFR {t.zoneTransfer}, NOTIFY {t.notify}
      </Text>
      {t.lastNotify ? (
        <Tooltip content={t.lastNotify.error ?? t.lastNotify.targets.join(', ')}>
          <Text size="sm" style={outcome(t.lastNotify.ok)}>
            notified {formatRelative(t.lastNotify.at)}
            {t.lastNotify.ok ? '' : ' (failed)'}
          </Text>
        </Tooltip>
      ) : null}
      {t.lastTransfer ? (
        <Text size="sm" color={t.lastTransfer.ok ? 'secondary' : undefined} style={t.lastTransfer.ok ? undefined : outcome(false)}>
          {t.lastTransfer.kind} to {t.lastTransfer.peer} {formatRelative(t.lastTransfer.at)}
        </Text>
      ) : null}
    </VStack>
  );
}

type Row = ZoneSummary & Record<string, unknown>;

const baseColumns: TableColumn<Row>[] = [
  {
    key: 'zoneName',
    header: 'Zone',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (z) => (
      <VStack gap={0.5}>
        <Link href={`/zones/${encodeURIComponent(z.name)}`} color="inherit" hasUnderline>
          <Text type="code" size="sm">
            {z.zoneName}
          </Text>
        </Link>
        {z.fallback ? <Text type="supporting">Public fallback</Text> : null}
      </VStack>
    ),
  },
  { key: 'type', header: 'Type', width: pixel(110), renderCell: (z) => <Badge variant="neutral" label={z.type} /> },
  {
    key: 'serial',
    header: 'Serial',
    align: 'end',
    width: pixel(120),
    renderCell: (z) => (
      <VStack gap={0.5}>
        <Text type="code" size="sm">{z.type === 'secondary' ? z.secondaryRuntime?.serial ?? '–' : z.type === 'stub' ? z.stubRuntime?.serial ?? '–' : z.serial}</Text>
        {z.type === 'secondary' && z.secondaryRuntime?.serial !== undefined && z.secondaryRuntime.state !== 'serving' ? <Text type="supporting">last snapshot</Text> : null}
        {z.type === 'stub' && z.stubRuntime?.serial !== undefined ? <Text type="supporting">last observed</Text> : null}
      </VStack>
    ),
  },
  {
    key: 'recordCount',
    header: 'Records',
    align: 'end',
    width: pixel(100),
    renderCell: (z) => <Text hasTabularNumbers>{formatExact(z.type === 'secondary' ? z.secondaryRuntime?.recordCount : z.type === 'stub' ? undefined : z.recordCount)}</Text>,
  },
  {
    key: 'ready',
    header: 'Availability',
    width: pixel(110),
    renderCell: (z) => z.type === 'secondary' ? <SecondaryAvailability runtime={z.secondaryRuntime} /> : z.type === 'stub' ? <StubAvailability runtime={z.stubRuntime} /> : (z.ready ? <Badge variant="success" label="Ready" /> : <Badge variant="error" label="Not ready" />),
  },
  { key: 'transfer', header: 'Transfers', width: proportional(1.75, { minWidth: 140 }), renderCell: (z) => <TransferCell z={z} /> },
  { key: 'managedBy', header: 'Managed by', width: proportional(1.5, { minWidth: 120 }), renderCell: (z) => <ManagedBadge owner={z.managedBy} inColumn /> },
];

export default function ZonesPage() {
  const api = useApi();
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const [creating, setCreating] = useNewParam();
  const importing = sp.get('import') === '1';
  const setImporting = (on: boolean) => setParams({ import: on ? '1' : null });
  const q = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list(), refetchInterval: 30_000 });
  const [deleting, setDeleting] = useState<Row | null>(null);
  const columns: TableColumn<Row>[] = [
    ...baseColumns,
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(124),
      renderCell: (z) => (
        <Button
          label="Delete"
          size="sm"
          variant="ghost"
          aria-label={`Delete zone ${z.zoneName}`}
          isDisabled={!!z.deleteRefusal || !can('zones', 'delete')}
          tooltip={
            z.deleteRefusal === 'default_zone'
              ? 'The root zone routes every other name; edit its settings on its page'
              : z.deleteRefusal === 'managed_by'
                ? 'The zone or some of its records are managed elsewhere'
                : !can('zones', 'delete')
                  ? 'Needs zones.delete'
                  : undefined
          }
          onClick={() => setDeleting(z)}
        />
      ),
    },
  ];
  return (
    <Page>
      <PageHeader
        title="Zones"
        description="Primary, secondary, forward and stub zones. Secondary and stub availability is shown for the responding node. Zones managed by a file or Kubernetes are read-only here."
        actions={
          <>
            <Button
              label="Import zone file"
              variant="secondary"
              isDisabled={!can('zones', 'write')}
              tooltip={can('zones', 'write') ? undefined : 'Needs zones.write'}
              onClick={() => setImporting(true)}
            />
            <Button label="New zone" variant="primary" isDisabled={!can('zones', 'write')} tooltip={can('zones', 'write') ? undefined : 'Needs zones.write'} onClick={() => setCreating(true)} />
          </>
        }
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <Card>
          <Table<Row>
            aria-label="Zones"
            density="compact"
            hasHover
            idKey="name"
            data={q.data.items}
            columns={columns}
            emptyState={<Text color="secondary">No zones yet.</Text>}
          />
        </Card>
      ) : null}
      {deleting ? <ZoneDeleteDialog zone={deleting.name} zoneName={deleting.zoneName} isOpen onClose={() => setDeleting(null)} /> : null}
      <ZoneImportDialog isOpen={importing} onClose={() => setImporting(false)} />
      <ZoneWizardDialog isOpen={creating} onClose={() => setCreating(false)} />
    </Page>
  );
}
