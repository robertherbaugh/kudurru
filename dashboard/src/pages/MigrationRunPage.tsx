import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { ProgressBar } from '@astryxdesign/core/ProgressBar';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useApi, useCan } from '../api/context';
import type { MigrationRun } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { SelectField } from '../components/fields/SelectField';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { migrationCounts, MigrationResults } from '../components/migration/results';
import { WizardDialog } from '../components/Wizard';
import { CutoverWizard } from '../components/wizards/MigrationHistory';
import type { PageProps } from '../routes';

type Zone = MigrationRun['data']['zones'][number] & Record<string, unknown>;
const zoneColumns: TableColumn<Zone>[] = [
  { key: 'zoneName', header: 'Zone', renderCell: (z) => <Text type="code">{z.zoneName}</Text> },
  { key: 'originalNameservers', header: 'Source authority', renderCell: (z) => <VStack gap={1}><Text>NS: {z.originalNameservers.join(', ') || 'default'}</Text><Text>SOA: {z.originalPrimary || 'default'}</Text></VStack> },
  { key: 'nameservers', header: 'Last recorded authority', renderCell: (z) => <VStack gap={1}><Text>NS: {z.nameservers.join(', ') || 'default'}</Text><Text>SOA: {z.primary || 'default'}</Text></VStack> },
];

export default function MigrationRunPage({ params }: PageProps) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const [cutover, setCutover] = useState(false);
  const [updated, setUpdated] = useState(false);
  const [filter, setFilter] = useState('all');
  const [search, setSearch] = useState('');
  const id = params.id ?? '';
  const query = useQuery({ queryKey: ['migration-runs', id], queryFn: () => api.imports.run(id), enabled: can('settings', 'write'), refetchInterval: (q) => q.state.data?.data.status === 'running' ? 1000 : false });
  const run = query.data;
  const status = run?.data.status;
  useEffect(() => {
    if (status && status !== 'running') {
      // Background writes may have changed cached objects or settings, including
      // successful writes from a partial import.
      void qc.invalidateQueries({ predicate: (q) => q.queryKey[0] !== 'migration-runs' });
    }
  }, [id, status, qc]);
  if (!can('settings', 'write')) return <Page><PageHeader title="Migration run" back={{ href: '/tools', label: 'Tools' }} /><Banner status="warning" title="Migration history requires settings.write" /></Page>;
  if (query.error || !run) return <Page><PageHeader title="Migration run" back={{ href: '/tools', label: 'Tools' }} />{query.error ? <ErrorBanner error={query.error} /> : <Loading label="Loading migration run" />}</Page>;
  const counts = migrationCounts(run);
  const running = run.data.status === 'running';
  const interrupted = run.data.status === 'interrupted';
  const problem = counts.failed > 0 || counts.notApplied > 0 || !!run.data.settingsError || !!run.data.journalError || !!run.data.failure || counts.pending > 0;
  const summary = `${counts.succeeded} succeeded, ${counts.failed} failed, ${counts.pending} ${running ? 'pending' : 'unconfirmed'}, ${counts.notApplied} not applied, ${counts.skipped} not selected.`;
  const items = run.data.items.filter((item) => {
    const matches = filter === 'all' || filter === 'error' && item.result === 'error' || filter === 'success' && ['created', 'updated', 'unchanged'].includes(item.result) || filter === 'skipped' && item.result === 'skipped' || filter === 'pending' && ['pending', 'running', 'not_applied', 'unconfirmed'].includes(item.result);
    return matches && `${item.kind} ${item.namespace ?? ''} ${item.name} ${item.error ?? ''}`.toLowerCase().includes(search.toLowerCase());
  });
  return <Page>
    <PageHeader title="Migration run" back={{ href: '/tools', label: 'Tools / Migration history' }} description={`${run.data.source} ${run.data.sourceURL ?? ''}`} meta={<Text type="code">{run.id}</Text>} actions={<Button label="Refresh report" variant="secondary" isLoading={query.isFetching} onClick={() => { void query.refetch(); }} />} />
    <Banner status={running ? 'info' : problem || interrupted ? 'warning' : 'success'} title={running ? 'Import in progress' : interrupted ? 'Import interrupted' : run.data.failure ? 'Import stopped before completion' : problem ? 'Import completed with errors' : 'Import completed'} description={`${summary} ${running ? 'You can leave this page or close the browser. The import continues on the server.' : 'Successful changes remain in place. Review DNS answers before switching clients.'}`} />
    <Panel title="Progress" headingId="migration-progress">
      <ProgressBar label="Selected items processed" value={counts.processed} max={Math.max(counts.selected, 1)} hasValueLabel formatValueLabel={(value, max) => `${value} of ${max}`} variant={running ? 'accent' : problem || interrupted ? 'warning' : 'success'} />
      <Text>Started {new Date(run.data.startedAt).toLocaleString()} by {run.data.actor}.{run.data.finishedAt ? ` Finished ${new Date(run.data.finishedAt).toLocaleString()}.` : ''}</Text>
      {run.data.failure ? <Text>{run.data.failure}</Text> : null}
      {run.data.journalError ? <Text>{run.data.journalError}</Text> : null}
      {run.data.settingsError ? <Text>Settings error: {run.data.settingsError}</Text> : null}
      {run.data.settingsPersisted === false ? <Banner status="warning" title="Settings are in memory only" description="Configure a settings file to preserve these settings after restart." /> : null}
    </Panel>
    <Panel title="Import results" headingId="migration-results" description="Each result is saved as the import progresses. A failed item does not roll back successful changes.">
      <HStack gap={3} wrap="wrap" vAlign="end">
        <TextField label="Search import results" value={search} onChange={setSearch} placeholder="Object name, kind, or error" />
        <SelectField label="Show results" value={filter} onChange={setFilter} options={[{ value: 'all', label: 'All items' }, { value: 'error', label: 'Errors' }, { value: 'success', label: 'Succeeded' }, { value: 'pending', label: 'Pending or not applied' }, { value: 'skipped', label: 'Not selected' }]} />
      </HStack>
      <MigrationResults items={items} />
    </Panel>
    {run.data.zones.length ? <Panel title="Zone authority" headingId="migration-authority" description="Last recorded authority. The decommission preview checks live zones before changing them.">
      <Table<Zone> aria-label="Tracked zone authority" data={run.data.zones} columns={zoneColumns} idKey="name" density="compact" />
      {updated ? <Banner status="success" title="Nameservers updated" description="Validate DNS answers and parent delegations before retiring the source." /> : null}
      <Button label="Decommission source nameservers" isDisabled={running} onClick={() => { setCutover(true); setUpdated(false); }} />
    </Panel> : null}
    {run.data.warnings.length ? <Banner status="warning" title={`Migration warnings (${run.data.warnings.length})`}><VStack gap={2}>{run.data.warnings.map((warning, i) => <Text as="p" key={i}>{warning}</Text>)}</VStack></Banner> : null}
    {run.data.cutovers.map((attempt, i) => <Panel key={i} title={`Cutover ${i + 1}: ${attempt.status}`} description={`${new Date(attempt.startedAt).toLocaleString()} by ${attempt.actor}`}>
      <Text>{attempt.addresses?.length ? `Serving IPs or VIPs: ${attempt.addresses.join(', ')}` : `Nameservers: ${attempt.nameservers.join(', ')}`}</Text>
      <MigrationResults items={attempt.items} label={`Cutover ${i + 1} results`} />
    </Panel>)}
    <WizardDialog isOpen={cutover} onClose={() => setCutover(false)} title="Decommission source nameservers" what="nameserver cutover"><CutoverWizard run={run} onDone={() => { setCutover(false); setUpdated(true); void qc.invalidateQueries({ queryKey: ['migration-runs'] }); }} /></WizardDialog>
  </Page>;
}
