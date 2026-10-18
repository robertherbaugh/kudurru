import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../../api/context';
import type { MigrationAuthorityChange, MigrationCutoverPlan, MigrationResult, MigrationRun } from '../../api/types';
import { ErrorBanner, errorText, Loading } from '../Feedback';
import { cleanList } from '../fields/StringListField';
import { CheckboxField } from '../fields/TextField';
import { Panel } from '../layout/Page';
import { Wizard, WizardDialog } from '../Wizard';
import { MigrationNameservers, migrationNameserverError } from './MigrationNameservers';

type ZoneRow = MigrationAuthorityChange & Record<string, unknown>;
const authorityColumns: TableColumn<ZoneRow>[] = [
  { key: 'zoneName', header: 'Zone', renderCell: (z) => <Text type="code">{z.zoneName}</Text> },
  { key: 'nameservers', header: 'Current NS', renderCell: (z) => <Text>{z.nameservers.join(', ') || '—'}</Text> },
  { key: 'primary', header: 'Current SOA primary', renderCell: (z) => <Text>{z.primary || '—'}</Text> },
  { key: 'error', header: 'Review', renderCell: (z) => <Text>{z.error || 'Ready'}</Text> },
];
type Item = MigrationRun['data']['items'][number] & Record<string, unknown>;
const itemColumns: TableColumn<Item>[] = [
  { key: 'kind', header: 'Kind', renderCell: (i) => <Text>{i.kind}</Text> },
  { key: 'name', header: 'Object', renderCell: (i) => <Text type="code">{i.namespace ? `${i.namespace}/` : ''}{i.name}</Text> },
  { key: 'result', header: 'Result', renderCell: (i) => <Text>{i.result}</Text> },
  { key: 'error', header: 'Error', renderCell: (i) => <Text>{i.error || '—'}</Text> },
];
function WriteErrors({ result }: { result: MigrationResult }) {
  const errors = [...result.items.filter((i) => i.result === 'error').map((i) => `${i.name}: ${i.error}`), result.journalError, result.settingsError].filter(Boolean);
  return errors.length ? <Banner status="error" title="Some changes failed" description={errors.join('\n')} /> : null;
}
const succeeded = (r: MigrationResult) => !r.journalError && !r.settingsError && r.items.every((i) => i.result !== 'error');

function CutoverWizard({ run, onDone }: { run: MigrationRun; onDone: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [nameservers, setNameservers] = useState<string[]>(['']);
  const [plan, setPlan] = useState<MigrationCutoverPlan>();
  const [reviewed, setReviewed] = useState(false);
  const [needsPreview, setNeedsPreview] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [result, setResult] = useState<MigrationResult>();
  return <Wizard label="Decommission source nameservers" finishLabel="Update nameservers" isFinishing={pending} error={error ? <ErrorBanner error={error} /> : undefined} steps={[
    { id: 'nameservers', label: 'Kudurru nameservers', description: 'Choose the DNS names kudurru will advertise for this migration’s zones.', content: <MigrationNameservers value={nameservers} onChange={(v) => { setNameservers(v); setPlan(undefined); setReviewed(false); setResult(undefined); }} />, validate: async () => {
      const problem = migrationNameserverError(nameservers); if (problem) return problem;
      try { setPlan(await api.imports.cutoverPreview(run.id, cleanList(nameservers))); setReviewed(false); setNeedsPreview(false); setError(undefined); return null; } catch (err) { return errorText(err); }
    } },
    { id: 'review', label: 'Review cutover', content: plan ? <VStack gap={4}>
      <VStack gap={2}><Text>New NS: {plan.nameservers.join(', ')}</Text><Text>New SOA primary: {plan.nameservers[0]}</Text></VStack>
      <Table<ZoneRow> aria-label="Nameserver changes" data={plan.zones} columns={authorityColumns} idKey="name" density="compact" />
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

function RunDetails({ id, onClose, onCutover }: { id: string; onClose: () => void; onCutover: (run: MigrationRun) => void }) {
  const api = useApi();
  const query = useQuery({ queryKey: ['migration-runs', id], queryFn: () => api.imports.run(id) });
  const run = query.data;
  return <Wizard label="Migration run" finishLabel="Close" onFinish={onClose} steps={[{ id: 'details', label: 'Run details', content: query.error ? <ErrorBanner error={query.error} /> : run ? <VStack gap={4}>
    <Text type="code">{run.id}</Text>
    <Text>{run.data.source} {run.data.sourceURL} — {run.data.status}. Started {new Date(run.data.startedAt).toLocaleString()} by {run.data.actor}.</Text>
    <Table<Item> aria-label="Import results" data={run.data.items} columns={itemColumns} idKey={(i) => `${i.kind}/${i.namespace}/${i.name}`} density="compact" />
    <Text type="supporting">Tracked authority at the latest recorded write. The cutover preview checks the live zones before changing them.</Text>
    <Table<ZoneRow> aria-label="Tracked zone authority" data={run.data.zones.map((z) => ({ ...z, resourceVersion: '', error: `Imported NS: ${z.originalNameservers.join(', ') || 'default'}; SOA primary: ${z.originalPrimary || 'default'}` }))} columns={authorityColumns} idKey="name" density="compact" />
    {run.data.settingsError ? <Banner status="error" title="Settings import failed" description={run.data.settingsError} /> : null}
    {run.data.warnings.length ? <Banner status="warning" title="Migration warnings" collapsible={false}><VStack gap={2}>{run.data.warnings.map((w, i) => <Text as="p" key={i}>{w}</Text>)}</VStack></Banner> : null}
    {run.data.cutovers.map((cutover, i) => <VStack gap={2} key={i}>
      <Text type="label">Cutover {i + 1}: {cutover.status} — {new Date(cutover.startedAt).toLocaleString()}</Text>
      <Text>NS: {cutover.nameservers.join(', ')}; SOA primary: {cutover.nameservers[0]}; requested by {cutover.actor}</Text>
      <Table<Item> aria-label={`Cutover ${i + 1} results`} data={cutover.items} columns={itemColumns} idKey="name" density="compact" />
    </VStack>)}
    <Button label="Decommission source nameservers" isDisabled={!run.data.zones.length} onClick={() => onCutover(run)} />
  </VStack> : <Loading label="Loading migration run" /> }]} />;
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
  const [details, setDetails] = useState<string>();
  const [cutover, setCutover] = useState<MigrationRun>();
  const [track, setTrack] = useState(false);
  const [updated, setUpdated] = useState(false);
  const query = useInfiniteQuery({ queryKey: ['migration-runs'], queryFn: ({ pageParam }) => api.imports.runs(pageParam), initialPageParam: undefined as string | undefined, getNextPageParam: (last) => last.continue || undefined, enabled, refetchInterval: 15000 });
  type Row = MigrationRun & Record<string, unknown>;
  const columns: TableColumn<Row>[] = [
    { key: 'startedAt', header: 'Started', renderCell: (r) => <Text>{new Date(r.data.startedAt).toLocaleString()}</Text> },
    { key: 'source', header: 'Source', renderCell: (r) => <Text>{r.data.source} {r.data.sourceURL}</Text> },
    { key: 'status', header: 'Import / latest cutover', renderCell: (r) => <Text>{r.data.status} / {r.data.cutovers.at(-1)?.status ?? 'No cutover'}</Text> },
    { key: 'zones', header: 'Zones', renderCell: (r) => <Text>{r.data.zones.length}</Text> },
    { key: 'actions', header: 'Actions', renderCell: (r) => <Button label={`Review migration ${r.id}`} size="sm" onClick={() => setDetails(r.id)}>Review run</Button> },
  ];
  return <Panel title="Migration history" headingId="migration-history" description="Review import results and replace source nameservers when kudurru is ready to take over.">
    {!enabled ? <Text>Migration history requires settings.write.</Text> : <VStack gap={4}>
      <Button label="Track previous imports" onClick={() => setTrack(true)} />
      {updated ? <Banner status="success" title="Nameservers updated" description="The cutover results are saved in migration history. Validate DNS answers and parent delegations before retiring the source." /> : null}
      {query.error ? <ErrorBanner error={query.error} /> : query.isPending ? <Loading label="Loading migration history" /> : <Table<Row> aria-label="Migration runs" data={query.data?.pages.flatMap((p) => p.items) ?? []} columns={columns} idKey="id" density="compact" />}
      {query.hasNextPage ? <Button label="Load more runs" isLoading={query.isFetchingNextPage} onClick={() => { void query.fetchNextPage(); }} /> : null}
    </VStack>}
    <WizardDialog isOpen={!!details} onClose={() => setDetails(undefined)} title="Migration run" what="migration review"><RunDetails id={details ?? ''} onClose={() => setDetails(undefined)} onCutover={(run) => { setDetails(undefined); setCutover(run); setUpdated(false); }} /></WizardDialog>
    <WizardDialog isOpen={!!cutover} onClose={() => setCutover(undefined)} title="Decommission source nameservers" what="nameserver cutover">{cutover ? <CutoverWizard run={cutover} onDone={() => { setCutover(undefined); setUpdated(true); }} /> : null}</WizardDialog>
    <WizardDialog isOpen={track} onClose={() => setTrack(false)} title="Track previous imports" what="migration tracking"><TrackWizard onDone={() => setTrack(false)} /></WizardDialog>
  </Panel>;
}
