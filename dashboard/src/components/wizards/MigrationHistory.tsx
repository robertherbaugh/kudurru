import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../../api/context';
import type { MigrationAuthorityChange, MigrationCutoverPlan, MigrationResult, MigrationRun } from '../../api/types';
import { useNavigate } from '../../lib/router';
import { migrationCounts } from '../migration/results';
import { ErrorBanner, errorText, Loading } from '../Feedback';
import { CheckboxField } from '../fields/TextField';
import { Panel } from '../layout/Page';
import { Wizard, WizardDialog } from '../Wizard';
import { MigrationNameservers, migrationNameserverError, initialNameservers, nameserverRequest } from './MigrationNameservers';

type ZoneRow = MigrationAuthorityChange & Record<string, unknown>;
const authorityColumns: TableColumn<ZoneRow>[] = [
  { key: 'zoneName', header: 'Zone', renderCell: (z) => <Text type="code">{z.zoneName}</Text> },
  { key: 'nameservers', header: 'Current NS', renderCell: (z) => <Text>{z.nameservers.join(', ') || '—'}</Text> },
  { key: 'primary', header: 'Current SOA primary', renderCell: (z) => <Text>{z.primary || '—'}</Text> },
  { key: 'error', header: 'Review', renderCell: (z) => <Text>{z.error || 'Ready'}</Text> },
];
type AddressRow = { id: string; name: string; address: string } & Record<string, unknown>;
const addressColumns: TableColumn<AddressRow>[] = [
  { key: 'name', header: 'Nameserver', renderCell: (r) => <Text type="code">{r.name}</Text> },
  { key: 'address', header: 'Address record', renderCell: (r) => <Text>{r.address}</Text> },
];
function WriteErrors({ result }: { result: MigrationResult }) {
  const errors = [...result.items.filter((i) => i.result === 'error').map((i) => `${i.name}: ${i.error}`), result.journalError, result.settingsError].filter(Boolean);
  return errors.length ? <Banner status="error" title="Some changes failed" description={errors.join('\n')} /> : null;
}
const succeeded = (r: MigrationResult) => !r.journalError && !r.settingsError && r.items.every((i) => i.result !== 'error');

export function CutoverWizard({ run, onDone }: { run: MigrationRun; onDone: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [nameservers, setNameservers] = useState(initialNameservers);
  const [plan, setPlan] = useState<MigrationCutoverPlan>();
  const [reviewed, setReviewed] = useState(false);
  const [needsPreview, setNeedsPreview] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [result, setResult] = useState<MigrationResult>();
  return <Wizard label="Decommission source nameservers" finishLabel="Update nameservers" isFinishing={pending} error={error ? <ErrorBanner error={error} /> : undefined} steps={[
    { id: 'nameservers', label: 'Kudurru nameservers', description: 'Choose the DNS names kudurru will advertise for this migration’s zones.', content: <MigrationNameservers value={nameservers} onChange={(v) => { setNameservers(v); setPlan(undefined); setReviewed(false); setResult(undefined); }} />, validate: async () => {
      const problem = migrationNameserverError(nameservers); if (problem) return problem;
      try { setPlan(await api.imports.cutoverPreview(run.id, nameserverRequest(nameservers).nameservers, nameserverRequest(nameservers).nameserverAddresses)); setReviewed(false); setNeedsPreview(false); setError(undefined); return null; } catch (err) { return errorText(err); }
    } },
    { id: 'review', label: 'Review cutover', content: plan ? <VStack gap={4}>
      <VStack gap={2}><Text>{plan.addresses?.length ? `Serving IPs or VIPs: ${plan.addresses.join(', ')}` : `New NS: ${plan.nameservers.join(', ')}; SOA primary: ${plan.nameservers[0]}`}</Text></VStack>
      <Table<ZoneRow> aria-label="Nameserver changes" data={plan.zones} columns={[
        authorityColumns[0]!,
        { key: 'nameservers', header: 'Current authority', renderCell: (z) => <VStack gap={1}><Text>NS: {z.nameservers.join(', ') || '—'}</Text><Text>SOA: {z.primary || '—'}</Text></VStack> },
        { key: 'targetNameservers', header: 'New NS (SOA primary first)', renderCell: (z) => <Text>{(z.targetNameservers ?? plan.nameservers).join(', ')}</Text> },
        authorityColumns[3]!,
      ]} idKey="name" density="compact" />
      {plan.addresses?.length ? <Table<AddressRow> aria-label="Generated nameserver addresses" data={plan.zones.flatMap((z) => (z.addressRecordSet?.spec.endpoints ?? []).map((e) => ({ id: `${z.name}/${e.dnsName}/${e.recordType}`, name: e.dnsName, address: `${e.recordType} ${e.targets?.join(', ') ?? ''}` })))} columns={addressColumns} idKey="id" density="compact" /> : null}
      <Banner status="warning" title="Before retiring the source" collapsible={false}><VStack gap={2}>{plan.warnings.map((w) => <Text as="p" key={w}>{w}</Text>)}</VStack></Banner>
      <CheckboxField label="I reviewed the nameserver changes" value={reviewed} onChange={setReviewed} />
      {result ? <WriteErrors result={result} /> : null}
    </VStack> : <Text>Go back to preview the cutover again.</Text>, validate: () => needsPreview ? 'Go back to preview the current state before retrying.' : !plan ? 'Preview the cutover again.' : !plan.zones.some((z) => !z.error) ? 'No zones are eligible. Review the errors above.' : !reviewed ? 'Review and acknowledge the nameserver changes.' : null },
  ]} onFinish={async () => {
    if (!plan) return;
    setPending(true); setError(undefined);
    try {
      const r = await api.imports.cutover(run.id, plan); setResult(r); await qc.invalidateQueries({ queryKey: ['migration-runs'] });
      if (succeeded(r)) onDone(); else { setNeedsPreview(true); setReviewed(false); setError(new Error('Some zones could not be updated. Go back to preview the current state before retrying.')); }
    } catch (err) { setError(err); } finally { setPending(false); }
  }} />;
}

function TrackWizard({ onDone }: { onDone: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const query = useQuery({ queryKey: ['migration-existing'], queryFn: () => api.imports.existing() });
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [result, setResult] = useState<MigrationResult>();
  const columns: TableColumn<ZoneRow>[] = [{ key: 'select', header: 'Track', renderCell: (z) => <CheckboxField label={`Track ${z.zoneName}`} value={selected.has(z.name)} onChange={(checked) => setSelected((old) => { const next = new Set(old); if (checked) next.add(z.name); else next.delete(z.name); return next; })} /> }, ...authorityColumns];
  return <Wizard label="Track previous imports" finishLabel="Track selected zones" isFinishing={pending} error={error ? <ErrorBanner error={error} /> : undefined} steps={[{ id: 'review', label: 'Review previous imports', description: 'Select API imports made before migration history was available. This records their current authority without reimporting records.', content: <VStack gap={4}>
    {query.error ? <ErrorBanner error={query.error} /> : query.isPending ? <Loading /> : <Table<ZoneRow> aria-label="Previous imports" data={query.data?.items ?? []} columns={columns} idKey="name" density="compact" />}
    {query.data?.items.length === 0 ? <Text>No untracked API imports were found.</Text> : null}
    {result ? <WriteErrors result={result} /> : null}
  </VStack>, validate: () => !selected.size ? 'Select at least one previous import.' : selected.size > 1000 ? 'Select at most 1000 zones at a time.' : null }]} onFinish={async () => {
    setPending(true); setError(undefined);
    try { const r = await api.imports.adopt((query.data?.items ?? []).filter((z) => selected.has(z.name))); setResult(r); await qc.invalidateQueries({ queryKey: ['migration-runs'] }); await qc.invalidateQueries({ queryKey: ['migration-existing'] }); if (succeeded(r)) onDone(); } catch (err) { setError(err); } finally { setPending(false); }
  }} />;
}

export function MigrationHistory() {
  const api = useApi();
  const can = useCan();
  const enabled = can('settings', 'write');
  const navigate = useNavigate();
  const [track, setTrack] = useState(false);
  const query = useInfiniteQuery({ queryKey: ['migration-runs'], queryFn: ({ pageParam }) => api.imports.runs(pageParam), initialPageParam: undefined as string | undefined, getNextPageParam: (last) => last.continue || undefined, enabled, refetchInterval: (q) => q.state.data?.pages.some((p) => p.items.some((r) => r.data.status === 'running')) ? 2000 : 15000 });
  type Row = MigrationRun & Record<string, unknown>;
  const columns: TableColumn<Row>[] = [
    { key: 'startedAt', header: 'Started', renderCell: (r) => <Text>{new Date(r.data.startedAt).toLocaleString()}</Text> },
    { key: 'source', header: 'Source', renderCell: (r) => <Text>{r.data.source} {r.data.sourceURL}</Text> },
    { key: 'status', header: 'Import / latest cutover', renderCell: (r) => <Text>{r.data.status} / {r.data.cutovers.at(-1)?.status ?? 'No cutover'}</Text> },
    { key: 'progress', header: 'Results', renderCell: (r) => { const counts = migrationCounts(r); return <Text>{counts.succeeded} succeeded · {counts.failed} failed · {counts.pending} pending · {counts.notApplied} not applied · {counts.skipped} skipped</Text>; } },
    { key: 'zones', header: 'Zones', renderCell: (r) => <Text>{r.data.zones.length}</Text> },
    { key: 'actions', header: 'Actions', renderCell: (r) => <Button label={`Review migration ${r.id}`} size="sm" onClick={() => navigate(`/tools/migrations/${encodeURIComponent(r.id)}`)}>Review run</Button> },
  ];
  return <Panel title="Migration history" headingId="migration-history" description="Review import results and replace source nameservers when kudurru is ready to take over.">
    {!enabled ? <Text>Migration history requires settings.write.</Text> : <VStack gap={4}>
      <Button label="Track previous imports" onClick={() => setTrack(true)} />
      {query.error ? <ErrorBanner error={query.error} /> : query.isPending ? <Loading label="Loading migration history" /> : <Table<Row> aria-label="Migration runs" data={query.data?.pages.flatMap((p) => p.items) ?? []} columns={columns} idKey="id" density="compact" />}
      {query.hasNextPage ? <Button label="Load more runs" isLoading={query.isFetchingNextPage} onClick={() => { void query.fetchNextPage(); }} /> : null}
    </VStack>}
    <WizardDialog isOpen={track} onClose={() => setTrack(false)} title="Track previous imports" what="migration tracking"><TrackWizard onDone={() => setTrack(false)} /></WizardDialog>
  </Panel>;
}
