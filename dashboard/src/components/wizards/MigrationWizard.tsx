import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../../api/context';
import type { AnyObject, MigrationPlan, MigrationRequest, MigrationRun, DNSZone } from '../../api/types';
import { useNavigate } from '../../lib/router';
import { ErrorBanner, errorText } from '../Feedback';
import { SelectField } from '../fields/SelectField';
import { cleanList, StringListField } from '../fields/StringListField';
import { CheckboxField, TextField } from '../fields/TextField';
import { Panel } from '../layout/Page';
import { MigrationNameservers, migrationNameserverError, initialNameservers, nameserverRequest } from './MigrationNameservers';
import { MigrationHistory } from './MigrationHistory';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';

type ObjectRow = { id: string; kind: string; name: string } & Record<string, unknown>;
type AuthorityRow = { name: string; names: string; primary: string; addresses: string } & Record<string, unknown>;
function authorityRows(plan: MigrationPlan): AuthorityRow[] {
  return plan.objects.filter((o): o is DNSZone => o.kind === 'DNSZone').map((zone) => {
    const names = zone.spec.nameservers ?? [];
    const addresses = plan.objects.filter((o) => o.kind === 'DNSRecordSet').flatMap((o) => o.spec.zoneRef.name === zone.metadata.name ? o.spec.endpoints : []).filter((e) => names.includes(e.dnsName) && (e.recordType === 'A' || e.recordType === 'AAAA')).map((e) => `${e.dnsName} ${e.recordType} ${e.targets.join(', ')}`).join('; ');
    return { name: zone.spec.zoneName, names: names.join(', '), primary: zone.spec.soa?.mname ?? '', addresses };
  });
}
const authorityColumns: TableColumn<AuthorityRow>[] = [
  { key: 'name', header: 'Zone', renderCell: (r) => <Text>{r.name}</Text> },
  { key: 'names', header: 'New NS', renderCell: (r) => <Text>{r.names}</Text> },
  { key: 'primary', header: 'SOA primary', renderCell: (r) => <Text>{r.primary}</Text> },
  { key: 'addresses', header: 'Address records', renderCell: (r) => <Text>{r.addresses || '—'}</Text> },
];
const columns: TableColumn<ObjectRow>[] = [
  { key: 'kind', header: 'Kind', renderCell: (r) => <Text>{r.kind}</Text> },
  { key: 'name', header: 'Name', renderCell: (r) => <Text type="code">{r.name}</Text> },
];

function objectRow(o: AnyObject): ObjectRow {
  const name = `${o.metadata.namespace ? `${o.metadata.namespace}/` : ''}${o.metadata.name}`;
  return { id: `${o.kind}/${name}`, kind: o.kind, name };
}

function MigrationWizard({ onImported }: { onImported: (run: MigrationRun) => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [source, setSource] = useState<MigrationRequest['source']>('technitium');
  const [url, setUrl] = useState('');
  const [credential, setCredential] = useState('');
  const [namespace, setNamespace] = useState('default');
  const [insecure, setInsecure] = useState(false);
  const [localZones, setLocalZones] = useState<string[]>([]);
  const [replaceNameservers, setReplaceNameservers] = useState(false);
  const [nameservers, setNameservers] = useState(initialNameservers);
  const [plan, setPlan] = useState<MigrationPlan>();
  const [reviewed, setReviewed] = useState(false);
  const [showObjects, setShowObjects] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [selectedSettings, setSelectedSettings] = useState<Set<string>>(new Set());
  const change = (f: () => void) => { f(); setPlan(undefined); setReviewed(false); setError(undefined); };
  const selectionChanged = () => setReviewed(false);
  const chosen = plan ? { ...plan, objects: plan.objects.filter((o) => selected.has(objectRow(o).id)) } : undefined;
  const objectColumns: TableColumn<ObjectRow>[] = [{ key: 'select', header: 'Import', renderCell: (r) => <CheckboxField label={`Import ${r.id}`} value={selected.has(r.id)} onChange={(checked) => { setSelected((old) => { const next = new Set(old); if (checked) next.add(r.id); else next.delete(r.id); return next; }); selectionChanged(); }} /> }, ...columns];
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
          const preview = await api.imports.preview({ source, url: url.trim(), credential, namespace: namespace.trim(), insecureSkipVerify: insecure, localZones: cleanList(localZones), ...(replaceNameservers ? nameserverRequest(nameservers) : { nameservers: [] }) });
          setPlan(preview);
          setSelected(new Set(preview.objects.map((o) => objectRow(o).id)));
          setSelectedSettings(new Set(Object.keys(preview.settings)));
          setReviewed(false);
          return null;
        } catch (err) { return errorText(err); }
      },
    },
    {
      id: 'review', label: 'Review', description: 'Review the converted configuration and any features that need attention before importing.',
      content: plan ? (
        <VStack gap={4}>
          <Text>{selected.size} of {plan.objects.length} objects and {selectedSettings.size} settings {selectedSettings.size === 1 ? 'section' : 'sections'} selected.</Text>
          <Text type="supporting">Choose what to import. Unselected items are recorded as skipped. Selected objects may reference other objects; make sure those dependencies are also selected or already exist in kudurru.</Text>
          <HStack gap={2} wrap="wrap">
            <Button label="Select all" variant="secondary" onClick={() => { setSelected(new Set(plan.objects.map((o) => objectRow(o).id))); setSelectedSettings(new Set(Object.keys(plan.settings))); selectionChanged(); }} />
            <Button label="Clear selection" variant="secondary" onClick={() => { setSelected(new Set()); setSelectedSettings(new Set()); selectionChanged(); }} />
          </HStack>
          <Text>{replaceNameservers ? nameservers.mode === 'addresses' ? `Generated nameservers will use these serving addresses: ${cleanList(nameservers.addresses).join(', ')}.` : `NS records will use ${cleanList(nameservers.names).join(', ')}; SOA primary will use ${cleanList(nameservers.names)[0]}.` : 'Source nameservers will be preserved. Use migration history to decommission them later.'}</Text>
          {replaceNameservers ? <Table<AuthorityRow> aria-label="Nameserver and address changes" density="compact" idKey="name" data={authorityRows(chosen ?? plan)} columns={authorityColumns} /> : null}
          <Table<ObjectRow> aria-label="Migration objects" density="compact" idKey="id" data={plan.objects.map(objectRow)} columns={objectColumns} />
          <Button label={showObjects ? 'Hide converted objects' : 'Show converted objects'} variant="secondary" onClick={() => setShowObjects((v) => !v)} />
          {showObjects ? <CodeBlock code={JSON.stringify(plan.objects, null, 2)} aria-label="Converted objects" width="100%" /> : null}
          {Object.keys(plan.settings).map((key) => <CheckboxField key={key} label={`Import settings: ${key}`} value={selectedSettings.has(key)} onChange={(checked) => { setSelectedSettings((old) => { const next = new Set(old); if (checked) next.add(key); else next.delete(key); return next; }); selectionChanged(); }} help={`Update the ${key} settings section. Settings are applied only after all selected objects succeed.`} />)}
          {showObjects && selectedSettings.size ? <CodeBlock code={JSON.stringify(Object.fromEntries(Object.entries(plan.settings).filter(([key]) => selectedSettings.has(key))), null, 2)} aria-label="Settings to import" width="100%" /> : null}
          <Banner status="warning" title={`Review before switching DNS: ${plan.warnings.length} warnings`}>
            <VStack gap={2}>{plan.warnings.map((w, i) => <Text as="p" key={i}>{w}</Text>)}</VStack>
          </Banner>
          <CheckboxField label="I reviewed the configuration and migration warnings" value={reviewed} onChange={setReviewed} />
        </VStack>
      ) : <Text>Go back to connect to the source server again.</Text>,
      validate: () => !plan ? 'Connect to the source server again.' : !selected.size && !selectedSettings.size ? 'Select at least one object or settings section.' : !reviewed ? 'Review the configuration and acknowledge the migration warnings.' : null,
    },
  ];
  return <Wizard label="Import from DNS server" steps={steps} finishLabel="Import configuration" isFinishing={pending} error={error ? <ErrorBanner error={error} /> : undefined} onFinish={async () => {
    if (!plan) return;
    setPending(true); setError(undefined);
    try {
      const imported = await api.imports.start({ ...plan, selection: { objects: [...selected], settings: [...selectedSettings] } });
      qc.setQueryData(['migration-runs', imported.id], imported);
      void qc.invalidateQueries({ queryKey: ['migration-runs'] });
      setCredential(''); onImported(imported);
    } catch (err) { setError(err); } finally { setPending(false); }
  }} />;
}

export function MigrationImport() {
  const can = useCan();
  const [open, setOpen] = useState(false);
  const navigate = useNavigate();
  return (
    <>
      <Panel title="Move from another DNS server" headingId="migration-title" description="Connect to Pi-hole v6 or Technitium, preview your configuration, then import lists, groups, supported zones and upstreams directly.">
        <Button label="Import from DNS server" variant="primary" isDisabled={!can('settings', 'write')} onClick={() => setOpen(true)} />
        <WizardDialog isOpen={open} onClose={() => setOpen(false)} title="Import from DNS server" what="migration">
          <MigrationWizard onImported={(r) => { setOpen(false); navigate(`/tools/migrations/${encodeURIComponent(r.id)}`); }} />
        </WizardDialog>
      </Panel>
      <MigrationHistory />
    </>
  );
}
