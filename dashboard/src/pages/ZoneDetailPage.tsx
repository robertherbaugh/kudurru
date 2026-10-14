// One zone: its records flattened across record sets, the typed record
// editor, BIND import/export, NOTIFY and the transfer status.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { FileInput } from '@astryxdesign/core/FileInput';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { ZoneImportResult, ZoneRecord } from '../api/types';
import { ErrorBanner, errorText, Loading, useNotify } from '../components/Feedback';
import { TextAreaField, TextField } from '../components/fields/TextField';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { ManagedBadge } from '../components/ManagedBadge';
import { SecondaryAvailability, SecondaryRuntimeSection } from '../components/SecondaryRuntime';
import { StubAvailability, StubRuntimeSection } from '../components/StubRuntime';
import { RecordEditor } from '../components/RecordEditor';
import { ZoneDeleteDialog } from '../components/ZoneDeleteDialog';
import { ZoneSettingsSection } from '../components/ZoneSettingsSection';
import { ownership, readOnlyReason } from '../lib/managedBy';
import { useNavigate } from '../lib/router';
import { dnsNameToOwner } from '../lib/rr';
import type { PageProps } from '../routes';
import { TransferCell } from './ZonesPage';
import { importSummary } from '../components/ZoneImportDialog';

/** Zone names in the page title keep the heading size in the code font. */
const MONO = { fontFamily: 'var(--font-family-code)' };

type RecordRow = ZoneRecord & Record<string, unknown>;

const recordKey = (r: RecordRow) => `${r.recordSet}/${r.dnsName}/${r.recordType}/${r.setIdentifier ?? ''}`;

function ImportDialog({ zone, open, onClose }: { zone: string; open: boolean; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const [file, setFile] = useState<File | null>(null);
  const [text, setText] = useState('');
  const [result, setResult] = useState<ZoneImportResult | null>(null);
  const m = useMutation({
    mutationFn: () => api.zones.importBind(zone, text),
    onSuccess: (r) => {
      setResult(r);
      notify.success(importSummary(r));
      void qc.invalidateQueries({ queryKey: qk.zone(zone) });
      void qc.invalidateQueries({ queryKey: qk.zones });
    },
    onError: (err) => notify.error(err, 'Import failed'),
  });
  const onFile = (files: File | File[] | null) => {
    const f = Array.isArray(files) ? (files[0] ?? null) : files;
    setFile(f);
    if (f) void f.text().then(setText);
  };
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={`Import a BIND zone file into ${zone}`}
      width={720}
      onSubmit={() => {
        if (text.trim()) m.mutate();
      }}
      footer={
        <>
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Import" variant="primary" isDisabled={!text.trim()} isLoading={m.isPending} />
        </>
      }
    >
      <FileInput label="Zone file" value={file} onChange={onFile} accept=".zone,.db,.txt,text/plain,text/dns" />
      <TextAreaField label="Or paste the zone text" value={text} onChange={setText} rows={12} hasSpellCheck={false} placeholder={'$ORIGIN example.internal.\nwww 300 IN A 192.0.2.10'} />
      <Text type="supporting" display="block">
        Records land in one record set managed by the API; SOA and apex NS are taken from the zone object.
      </Text>
      {result ? (
        <Banner status={result.warnings.length ? 'warning' : 'success'} title={importSummary(result)} description={result.warnings.length ? result.warnings.join('; ') : undefined} />
      ) : null}
    </FormDialog>
  );
}

function ExportDialog({ zone, open, onClose }: { zone: string; open: boolean; onClose: () => void }) {
  const api = useApi();
  const q = useQuery({ queryKey: [...qk.zone(zone), 'export'], queryFn: () => api.zones.exportText(zone), enabled: open });
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={`${zone} as a BIND zone file`}
      width={760}
      footer={
        <>
          <Button label="Download" variant="secondary" href={api.zones.exportURL(zone)} target="_blank" rel="noopener" />
          <Button label="Close" variant="primary" onClick={onClose} />
        </>
      }
    >
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? <CodeBlock aria-label="Zone file" code={q.data} language="plaintext" maxHeight={480} width="100%" /> : null}
    </FormDialog>
  );
}

// Object names never end with a dot, so a trailing dot means the URL holds
// the zone's apex (a link someone typed or copied from dig output). Resolve
// it to the DNSZone object instead of asking the API for an object that
// cannot exist; the API keeps the object name as the only {zone} key because
// zone-scoped RBAC grants are keyed by it.
function ApexRedirect({ apex }: { apex: string }) {
  const api = useApi();
  const navigate = useNavigate();
  const q = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  const want = apex.toLowerCase();
  const match = q.data?.items.find((z) => z.zoneName.toLowerCase() === want || `${z.zoneName.toLowerCase()}.` === want);
  useEffect(() => {
    if (match) navigate(`/zones/${encodeURIComponent(match.name)}`, { replace: true });
  }, [match, navigate]);
  if (q.error) return <ErrorBanner error={q.error} />;
  if (q.data && !match) {
    return (
      <Page>
        <PageHeader title={apex} />
        <Banner status="warning" title="No such zone" description={`No zone has the apex ${apex}.`} />
        <Text>
          <Link href="/zones">All zones</Link>
        </Text>
      </Page>
    );
  }
  return <Loading />;
}

export default function ZoneDetailPage({ params }: PageProps) {
  const zone = params.zone ?? '';
  if (zone.endsWith('.')) return <ApexRedirect apex={zone} />;
  return <ZoneDetail zone={zone} />;
}

function ZoneDetail({ zone }: { zone: string }) {
  const api = useApi();
  const navigate = useNavigate();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [editing, setEditing] = useState<ZoneRecord | 'new' | null>(null);
  const [importing, setImporting] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [filter, setFilter] = useState('');
  const [deleting, setDeleting] = useState(false);
  const [confirmRecord, setConfirmRecord] = useState<ZoneRecord | null>(null);

  const z = useQuery({ queryKey: qk.zone(zone), queryFn: () => api.zones.get(zone), refetchInterval: 30_000 });
  const secondary = z.data?.type === 'secondary';
  const stub = z.data?.type === 'stub';
  const primary = z.data?.type === 'primary';
  const recs = useQuery({ queryKey: qk.records(zone), queryFn: () => api.zones.records(zone), enabled: primary });
  const own = ownership(z.data?.managedBy);
  const zoneEditable = own.editable && z.data?.type === 'primary';
  const canWrite = can('records', 'write');

  const rows = useMemo(() => {
    const all = recs.data?.items ?? [];
    const f = filter.trim().toLowerCase();
    return f ? all.filter((r) => r.dnsName.toLowerCase().includes(f) || r.recordType.toLowerCase() === f || r.targets.some((t) => t.toLowerCase().includes(f))) : all;
  }, [recs.data, filter]);

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: qk.records(zone) });
    void qc.invalidateQueries({ queryKey: qk.zone(zone) });
    void qc.invalidateQueries({ queryKey: qk.zones });
  };

  const del = useMutation({
    mutationFn: (r: ZoneRecord) => api.zones.deleteRecord(zone, r.dnsName, r.recordType, r.recordSet, r.setIdentifier),
    onSuccess: (_x, r) => {
      notify.success(`Deleted ${r.dnsName} ${r.recordType}`);
      invalidate();
    },
    onError: (err) => notify.error(err, 'Delete failed'),
  });
  const notifyM = useMutation({
    mutationFn: () => api.zones.notify(zone),
    onSuccess: (r) => {
      notify.success(r.targets.length ? `NOTIFY sent to ${r.targets.join(', ')}` : 'No NOTIFY targets configured');
      invalidate();
    },
    onError: (err) => notify.error(err, 'NOTIFY failed'),
  });

  if (z.error) {
    return (
      <Page>
        <PageHeader title={`Zone ${zone}`} />
        <ErrorBanner error={z.error} />
        <Text>
          <Link href="/zones">Back to zones</Link>
        </Text>
      </Page>
    );
  }
  const zoneName = z.data?.zoneName ?? zone;
  const columns: TableColumn<RecordRow>[] = [
    {
      key: 'dnsName',
      header: 'Name',
      width: proportional(2, { minWidth: 150 }),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <HStack gap={1} vAlign="center" wrap="wrap">
            <Text type="code" size="sm">{dnsNameToOwner(r.dnsName, zoneName)}</Text>
            {r.kudurru?.disabled ? <Badge variant="warning" label="disabled" /> : null}
          </HStack>
          {r.kudurru?.comment ? <Text type="supporting">{r.kudurru.comment}</Text> : null}
        </VStack>
      ),
    },
    { key: 'recordType', header: 'Type', width: pixel(104), renderCell: (r) => <Text>{r.recordType}</Text> },
    {
      key: 'recordTTL',
      header: 'TTL',
      align: 'end',
      width: pixel(80),
      renderCell: (r) => (r.recordTTL !== undefined ? <Text hasTabularNumbers>{r.recordTTL}</Text> : <Text color="secondary">default</Text>),
    },
    {
      key: 'targets',
      header: 'Value',
      width: proportional(3, { minWidth: 180 }),
      renderCell: (r) => (
        <VStack gap={0}>
          {r.targets.map((t) => (
            <Text key={t} type="code" size="sm" maxLines={2} wordBreak="break-all">
              {t}
            </Text>
          ))}
        </VStack>
      ),
    },
    {
      key: 'recordSet',
      header: 'Record set',
      width: proportional(2, { minWidth: 150 }),
      renderCell: (r) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Text size="sm">{r.recordSet}</Text>
          {r.managedBy && !ownership(r.managedBy).editable ? <ManagedBadge owner={r.managedBy} /> : null}
        </HStack>
      ),
    },
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(176),
      renderCell: (r) => {
        const editable = zoneEditable && ownership(r.managedBy).editable && canWrite;
        return (
          <HStack gap={0.5}>
            <Button label="Edit" size="sm" variant="ghost" isDisabled={!editable} onClick={() => setEditing(r)} />
            <Button
              label="Delete"
              size="sm"
              variant="ghost"
              isDisabled={!editable || !can('records', 'delete')}
              onClick={() => setConfirmRecord(r)}
            />
          </HStack>
        );
      },
    },
  ];
  return (
    <Page>
      <PageHeader
        back={{ href: '/zones', label: 'Zones' }}
        title={
          <Text type="inherit" style={MONO}>
            {zoneName}
          </Text>
        }
        meta={
          z.data ? (
            <>
              <Badge variant="neutral" label={z.data.type} />
              {secondary ? <SecondaryAvailability runtime={z.data.secondaryRuntime} /> : stub ? <StubAvailability runtime={z.data.stubRuntime} /> : (
                <>
                  <Text size="sm">serial {z.data.serial}</Text>
                  <Text size="sm">{z.data.recordCount} records</Text>
                  {z.data.ready ? <Badge variant="success" label="Ready" /> : <Badge variant="error" label="Not ready" />}
                </>
              )}
              <ManagedBadge owner={z.data.managedBy} />
            </>
          ) : null
        }
        actions={
          <>
            {z.data && primary ? (
              <>
                <Button label="Add record" variant="primary" isDisabled={!zoneEditable || !canWrite} onClick={() => setEditing('new')} />
                <Button label="Import BIND" variant="secondary" isDisabled={!zoneEditable || !canWrite} onClick={() => setImporting(true)} />
                <Button label="Export" variant="secondary" onClick={() => setExporting(true)} />
                <Button label="Send NOTIFY" variant="ghost" isDisabled={!can('zones', 'transfer')} isLoading={notifyM.isPending} onClick={() => notifyM.mutate()} />
              </>
            ) : null}
            <Button
              label="Delete zone"
              variant="destructive"
              isDisabled={!z.data || !!z.data.deleteRefusal || !can('zones', 'delete')}
              tooltip={
                z.data?.deleteRefusal === 'default_zone'
                  ? 'The root zone routes every other name; edit its settings instead'
                  : z.data?.deleteRefusal === 'managed_by'
                    ? 'The zone or some of its records are managed elsewhere'
                    : !can('zones', 'delete')
                      ? 'Needs zones.delete'
                      : undefined
              }
              onClick={() => setDeleting(true)}
            />
          </>
        }
      />
      {z.data && !own.editable ? <Banner status="info" title={`Managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      {z.data && own.editable && z.data.type !== 'primary' ? (
        <Banner status="info" title={`A ${z.data.type} zone`} description={secondary ? 'Records are transferred from the primary and are read-only. Refresh, retry, expiry and TTLs come from the primary. Transferred contents are not listed as editable record sets.' : stub ? 'Authorities are discovered from the bootstrap servers. Queries go directly to them; this node does not keep an editable copy of the zone.' : 'Queries go to the configured upstream group; records are managed there.'} />
      ) : null}
      {z.data ? <ZoneSettingsSection zone={zone} own={own} /> : null}
      {secondary ? <SecondaryRuntimeSection runtime={z.data?.secondaryRuntime} /> : null}
      {stub ? <StubRuntimeSection runtime={z.data?.stubRuntime} /> : null}
      {primary && z.data?.transfer ? (
        <Panel title="Transfers and NOTIFY" headingId="xfr">
          <TransferCell z={z.data} />
        </Panel>
      ) : null}

      {primary ? <TextField label="Filter records" isLabelHidden startIcon="search" placeholder="Filter by name, type or value" value={filter} onChange={setFilter} hasClear /> : null}
      {primary && recs.error ? <ErrorBanner error={recs.error} /> : null}
      {primary && recs.isLoading ? <Loading /> : null}
      {primary && recs.data ? (
        <Card>
          <Table<RecordRow>
            aria-label={`Records of ${zoneName}`}
            density="compact"
            hasHover
            idKey={recordKey}
            data={rows as RecordRow[]}
            columns={columns}
            emptyState={<Text color="secondary">{filter ? 'No records match.' : 'No records yet.'}</Text>}
          />
        </Card>
      ) : null}

      <FormDialog
        isOpen={editing !== null}
        onClose={() => setEditing(null)}
        title={editing === null ? '' : editing === 'new' ? `Add a record to ${zoneName}` : `Edit ${editing.dnsName} ${editing.recordType}`}
        width={720}
      >
        {editing !== null ? (
          <RecordEditor
            zoneName={zoneName}
            initial={editing === 'new' ? undefined : editing}
            defaultRecordSet={`api-${zone}`}
            onCancel={() => setEditing(null)}
            onSubmit={async (ep, recordSet) => {
              try {
                await api.zones.upsertRecord(zone, ep, recordSet);
              } catch (err) {
                throw new Error(errorText(err), { cause: err });
              }
              notify.success(`Saved ${ep.dnsName} ${ep.recordType}`);
              setEditing(null);
              invalidate();
            }}
          />
        ) : null}
      </FormDialog>
      <AlertDialog
        isOpen={confirmRecord !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmRecord(null);
        }}
        title={confirmRecord ? `Delete ${confirmRecord.dnsName} ${confirmRecord.recordType}?` : 'Delete the record?'}
        description={confirmRecord ? `Every ${confirmRecord.recordType} value of ${confirmRecord.dnsName} in record set ${confirmRecord.recordSet} is removed.` : ''}
        cancelLabel="Keep it"
        actionLabel="Delete record"
        actionVariant="destructive"
        isActionLoading={del.isPending}
        onAction={() => {
          if (confirmRecord) del.mutate(confirmRecord, { onSettled: () => setConfirmRecord(null) });
        }}
      />
      <ZoneDeleteDialog zone={zone} zoneName={zoneName} isOpen={deleting} onClose={() => setDeleting(false)} onDeleted={() => navigate('/zones')} />
      {primary ? <ImportDialog zone={zone} open={importing} onClose={() => setImporting(false)} /> : null}
      {primary ? <ExportDialog zone={zone} open={exporting} onClose={() => setExporting(false)} /> : null}
    </Page>
  );
}
