import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../../api/context';
import type { AnyObject, MigrationPlan, MigrationRequest, MigrationResult } from '../../api/types';
import { ErrorBanner, errorText } from '../Feedback';
import { SelectField } from '../fields/SelectField';
import { cleanList, StringListField } from '../fields/StringListField';
import { CheckboxField, TextField } from '../fields/TextField';
import { Panel } from '../layout/Page';
import { MigrationNameservers, migrationNameserverError } from './MigrationNameservers';
import { MigrationHistory } from './MigrationHistory';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';

type ObjectRow = { id: string; kind: string; name: string } & Record<string, unknown>;
const columns: TableColumn<ObjectRow>[] = [
  { key: 'kind', header: 'Kind', renderCell: (r) => <Text>{r.kind}</Text> },
  { key: 'name', header: 'Name', renderCell: (r) => <Text type="code">{r.name}</Text> },
];

function objectRow(o: AnyObject): ObjectRow {
  const name = `${o.metadata.namespace ? `${o.metadata.namespace}/` : ''}${o.metadata.name}`;
  return { id: `${o.kind}/${name}`, kind: o.kind, name };
}

function MigrationWizard({ onImported }: { onImported: (result: MigrationResult) => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [source, setSource] = useState<MigrationRequest['source']>('technitium');
  const [url, setUrl] = useState('');
  const [credential, setCredential] = useState('');
  const [namespace, setNamespace] = useState('default');
  const [insecure, setInsecure] = useState(false);
  const [localZones, setLocalZones] = useState<string[]>([]);
  const [replaceNameservers, setReplaceNameservers] = useState(false);
  const [nameservers, setNameservers] = useState<string[]>(['']);
  const [plan, setPlan] = useState<MigrationPlan>();
  const [reviewed, setReviewed] = useState(false);
  const [showObjects, setShowObjects] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [result, setResult] = useState<MigrationResult>();
  const change = (f: () => void) => { f(); setPlan(undefined); setReviewed(false); setResult(undefined); setError(undefined); };
  const steps: WizardStep[] = [
    {
      id: 'connect', label: 'Connect', description: 'Read configuration from the server you are moving from.',
      content: (
        <VStack gap={4}>
          <SelectField label="Source server" value={source} options={[{ value: 'technitium', label: 'Technitium' }, { value: 'pihole', label: 'Pi-hole v6' }]} onChange={(v) => change(() => setSource(v as MigrationRequest['source']))} />
          <TextField label="Server URL" value={url} onChange={(v) => change(() => setUrl(v))} placeholder={source === 'technitium' ? 'http://dns.local:5380' : 'http://pi.hole'} help="Must be reachable from this kudurru server. Include any reverse-proxy prefix." />
          <TextField label={source === 'technitium' ? 'API token' : 'Application password'} value={credential} onChange={(v) => change(() => setCredential(v))} type="password" autoComplete="off" help={source === 'technitium' ? 'Create a token in Technitium with access to settings, allowed/blocked zones and the zones you want to move.' : 'Generate an application password in Pi-hole Settings → Web interface / API. Pi-hole v6 uses this password to open a temporary API session.'} />
          <TextField label="Destination namespace" value={namespace} onChange={(v) => change(() => setNamespace(v))} help="Use the namespace of your existing default policy." />
          {source === 'pihole' ? <StringListField label="Local DNS suffixes" value={localZones} onChange={(v) => change(() => setLocalZones(v))} placeholder="home.arpa" description="Optional: defaults to Pi-hole’s configured local domain. Each imported suffix becomes authoritative; missing names under it will stop forwarding upstream." /> : null}
          <CheckboxField label="Update nameservers during import" value={replaceNameservers} onChange={(v) => change(() => setReplaceNameservers(v))} help="Use kudurru’s authoritative DNS names now, or preserve the source names and decommission later from migration history." />
          {replaceNameservers ? <MigrationNameservers value={nameservers} onChange={(v) => change(() => setNameservers(v))} /> : null}
          <CheckboxField label="Trust a self-signed source certificate" value={insecure} onChange={(v) => change(() => setInsecure(v))} />
        </VStack>
      ),
      validate: async () => {
        if (!credential.trim()) return 'Enter the source API token or application password.';
        try { const u = new URL(url); if (!['http:', 'https:'].includes(u.protocol) || u.username || u.password || u.search || u.hash) return 'Enter an HTTP or HTTPS server URL without credentials or query parameters.'; } catch { return 'Enter a valid server URL.'; }
        if (!namespace.trim()) return 'Enter a destination namespace.';
        if (replaceNameservers) { const problem = migrationNameserverError(nameservers); if (problem) return problem; }
        setError(undefined);
        try {
          const preview = await api.imports.preview({ source, url: url.trim(), credential, namespace: namespace.trim(), insecureSkipVerify: insecure, localZones: cleanList(localZones), nameservers: replaceNameservers ? cleanList(nameservers) : [] });
          setPlan(preview);
          setReviewed(false);
          return null;
        } catch (err) { return errorText(err); }
      },
    },
    {
      id: 'review', label: 'Review', description: 'Review the converted configuration and any features that need attention before importing.',
      content: plan ? (
        <VStack gap={4}>
          <Text>{plan.objects.length} objects are ready to import.</Text>
          <Text>{replaceNameservers ? `NS records will use ${cleanList(nameservers).join(', ')}; SOA primary will use ${cleanList(nameservers)[0]}.` : 'Source nameservers will be preserved. Use migration history to decommission them later.'}</Text>
          <Table<ObjectRow> aria-label="Migration objects" density="compact" idKey="id" data={plan.objects.map(objectRow)} columns={columns} />
          <Button label={showObjects ? 'Hide converted objects' : 'Show converted objects'} variant="secondary" onClick={() => setShowObjects((v) => !v)} />
          {showObjects ? <CodeBlock code={JSON.stringify(plan.objects, null, 2)} aria-label="Converted objects" width="100%" /> : null}
          {Object.keys(plan.settings).length ? <CodeBlock code={JSON.stringify(plan.settings, null, 2)} aria-label="Settings to import" width="100%" /> : null}
          <Banner status="warning" title="Review before switching DNS" collapsible={false}>
            <VStack gap={2}>{plan.warnings.map((w, i) => <Text as="p" key={i}>{w}</Text>)}</VStack>
          </Banner>
          <CheckboxField label="I reviewed the configuration and migration warnings" value={reviewed} onChange={setReviewed} />
          {result ? <Banner status="error" title="Some changes could not be imported" description={[...result.items.filter((r) => r.result === 'error').map((r) => `${r.kind}/${r.name}: ${r.error}`), result.settingsError, result.journalError].filter(Boolean).join('\n')} /> : null}
        </VStack>
      ) : <Text>Go back to connect to the source server again.</Text>,
      validate: () => !plan ? 'Connect to the source server again.' : !reviewed ? 'Review the configuration and acknowledge the migration warnings.' : null,
    },
  ];
  return <Wizard label="Import from DNS server" steps={steps} finishLabel="Import configuration" isFinishing={pending} error={error ? <ErrorBanner error={error} /> : undefined} onFinish={async () => {
    if (!plan) return;
    setPending(true); setError(undefined);
    try {
      const imported = await api.imports.apply(plan);
      setResult(imported);
      await qc.invalidateQueries();
      if (!imported.settingsError && !imported.journalError && imported.items.every((r) => r.result !== 'error')) { setCredential(''); onImported(imported); }
    } catch (err) { setError(err); } finally { setPending(false); }
  }} />;
}

export function MigrationImport() {
  const can = useCan();
  const [open, setOpen] = useState(false);
  const [result, setResult] = useState<MigrationResult>();
  return (
    <>
      <Panel title="Move from another DNS server" headingId="migration-title" description="Connect to Pi-hole v6 or Technitium, preview your configuration, then import lists, groups, supported zones and upstreams directly.">
        <Button label="Import from DNS server" variant="primary" isDisabled={!can('settings', 'write')} onClick={() => setOpen(true)} />
        {result ? <Banner status="success" title="Configuration imported" description={`${result.items.length} objects processed. Validate DNS answers before switching clients.${result.settings?.persisted === false ? ' Settings are currently in memory only; configure a settings file to keep them after restart.' : ''}`} /> : null}
        <WizardDialog isOpen={open} onClose={() => setOpen(false)} title="Import from DNS server" what="migration">
          <MigrationWizard onImported={(r) => { setResult(r); setOpen(false); }} />
        </WizardDialog>
      </Panel>
      <MigrationHistory />
    </>
  );
}
