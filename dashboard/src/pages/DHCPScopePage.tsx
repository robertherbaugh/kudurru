import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Section } from '@astryxdesign/core/Section';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Table, proportional, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useDeferredValue, useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { DHCPConfig, DHCPLease, DHCPPoolOptions, DHCPReservation } from '../api/types';
import { DHCPSavedScope, DHCPSetupNotices, useDHCPSetup } from '../components/DHCPSetup';
import { DHCPAdvancedFields, advancedOptionsError } from '../components/DHCPAdvancedFields';
import { DHCPFailoverStatus } from '../components/DHCPFailover';
import { isKube, managedText } from '../components/DHCPSetupFields';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { DurationField, durationError } from '../components/fields/DurationField';
import { SelectField } from '../components/fields/SelectField';
import { StringListField, cleanList } from '../components/fields/StringListField';
import { SwitchField, TextAreaField, TextField } from '../components/fields/TextField';
import { formatError } from '../components/fields/formats';
import { PageHeader } from '../components/layout/Page';
import { formatRelative, parseDuration } from '../lib/format';
import type { PageProps } from '../routes';
import { DHCPFrame, dhcpServingText } from './DHCPPage';

const ipError = (v: string) => v.trim() ? formatError('ipv4', v) : 'An IPv4 address is required';
const hostnameError = (v: string) => !v || (v.length <= 253 && v.replace(/\.$/, '').split('.').every((p) => /^[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?$/i.test(p))) ? undefined : 'Use a hostname such as printer or printer.home.arpa';
function macError(v: string) {
  return /^(?:[\da-f]{2}:){5}[\da-f]{2}$/i.test(v) && (parseInt(v.slice(0, 2), 16) & 1) === 0 && v !== '00:00:00:00:00:00'
    ? undefined : 'Use a unicast Ethernet MAC, for example 02:00:00:00:00:10';
}

function useRefreshOnConflict(scope: string) {
  const qc = useQueryClient();
  return (error: Error) => {
    if (error instanceof ApiError && error.status === 412) {
      void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'config') });
    }
  };
}

function OptionsEditor({ config, saved }: { config: DHCPConfig; saved: (v: DHCPConfig) => void }) {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const onConflict = useRefreshOnConflict(config.scope);
  const [base, setBase] = useState(config);
  const [draft, setDraft] = useState(config.options);
  const [submitted, setSubmitted] = useState(false);
  const stale = config.etag !== base.etag;
  const writable = config.management === 'api' && can('dhcp', 'write');
  const locked = (key: keyof DHCPPoolOptions) => !writable || config.envLocked.includes(key);
  const update = <K extends keyof DHCPPoolOptions>(key: K, value: DHCPPoolOptions[K]) => setDraft((old) => ({ ...old, [key]: value }));
  const errors: Partial<Record<keyof DHCPPoolOptions, string>> = {};
  for (const key of ['rangeStart', 'rangeEnd', 'router'] as const) {
    const err = ipError(draft[key]);
    if (err) errors[key] = err;
  }
  const servers = cleanList(draft.dnsServers);
  if (!servers.length || servers.some((ip) => ipError(ip))) errors.dnsServers = 'Specify reachable IPv4 DNS servers';
  const leaseError = durationError(draft.leaseDuration, { min: 60 });
  if (leaseError || parseDuration(draft.leaseDuration) > 30 * 86400 || !Number.isInteger(parseDuration(draft.leaseDuration))) errors.leaseDuration = leaseError ?? 'Use whole seconds, up to 30 days';
  const ttlError = durationError(draft.dnsTTL, { min: 1 });
  if (ttlError || parseDuration(draft.dnsTTL) > 3600 || !Number.isInteger(parseDuration(draft.dnsTTL))) errors.dnsTTL = ttlError ?? 'Use whole seconds, up to one hour';
  const domainError = hostnameError(draft.domainName);
  if (domainError || (draft.registerDNS && !draft.domainName.trim())) errors.domainName = domainError ?? 'A domain is required for DNS registration';
  const advancedError = advancedOptionsError(draft);
  const mutation = useMutation({
    onError: onConflict,
    mutationFn: () => api.dhcp.update(config.scope, { ...draft, dnsServers: servers }, base.etag),
    onSuccess: (next) => { saved(next); setBase(next); setDraft(next.options); setSubmitted(false); notify.success('DHCP settings saved and applied'); },
  });
  const reset = () => { setBase(config); setDraft(config.options); setSubmitted(false); mutation.reset(); };
  return <Section padding={0} variant="transparent">
    <VStack as="form" gap={4} onSubmit={(e) => { e.preventDefault(); setSubmitted(true); if (writable && !stale && !mutation.isPending && !Object.keys(errors).length && !advancedError) mutation.mutate(); }}>
      <Heading level={2}>Network options</Heading>
      <Text type="supporting">Changes apply to this scope immediately. Existing leases keep their recorded expiration.</Text>
      {stale ? <Banner status="warning" title="This scope has changed" description="Reload the saved values before making another change. Your draft is still shown."
        endContent={<Button label="Reload saved values" onClick={reset} />} /> : null}
      {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
      <HStack gap={4} wrap="wrap" vAlign="start">
        {(['rangeStart', 'rangeEnd', 'router'] as const).map((key) => <TextField key={key} label={{ rangeStart: 'First address', rangeEnd: 'Last address', router: 'Router' }[key]}
          value={draft[key]} onChange={(v) => update(key, v)} placeholder="192.0.2.100" mono isDisabled={locked(key)}
          error={submitted ? errors[key] : undefined} help={config.envLocked.includes(key) ? 'Set by an environment variable.' : undefined} />)}
      </HStack>
      <StringListField label="DNS servers" value={draft.dnsServers} onChange={(v) => update('dnsServers', v)} addLabel="Add DNS server" placeholder="192.0.2.2"
        itemError={ipError} isDisabled={locked('dnsServers')} error={submitted ? errors.dnsServers : undefined}
        description="Addresses advertised to clients. Make sure a DNS server is reachable at each address." />
      <TextField label="Domain name" value={draft.domainName} onChange={(v) => update('domainName', v)} placeholder="home.arpa"
        isDisabled={locked('domainName')} error={submitted ? errors.domainName : undefined} help="Advertised to clients and used for registered lease names." />
      <DurationField label="Lease duration" value={draft.leaseDuration} onChange={(v) => update('leaseDuration', v)} isDisabled={locked('leaseDuration')}
        error={submitted ? errors.leaseDuration : undefined} description="Between one minute and 30 days." />
      <SwitchField label="Register lease names in DNS" value={draft.registerDNS} onChange={(v) => update('registerDNS', v)} isDisabled={locked('registerDNS')}
        help="Serve A and PTR records for active leases. Specific configured DNS zones take precedence; use a domain dedicated to DHCP names." />
      <DurationField label="Lease record TTL" value={draft.dnsTTL} onChange={(v) => update('dnsTTL', v)} isDisabled={locked('dnsTTL')}
        error={submitted ? errors.dnsTTL : undefined} description="One second to one hour, limited by each lease's remaining lifetime." />
      <DHCPAdvancedFields value={draft} onChange={setDraft} disabled={!writable} locked={locked} />
      {submitted && advancedError ? <Banner status="error" title="Check additional options" description={advancedError} /> : null}
      <HStack gap={2}>
        <Button label="Save network options" type="submit" variant="primary" isLoading={mutation.isPending} isDisabled={!writable || stale}
          tooltip={!writable ? config.managedBy ? managedText(config.managedBy) : config.management === 'file' ? 'Managed by the configuration file' : 'Requires dhcp.write' : undefined} />
        <Button label="Reset changes" onClick={reset} isDisabled={mutation.isPending} />
      </HStack>
    </VStack>
  </Section>;
}

function ReservationDialog({ config, reservation, close, saved }: { config: DHCPConfig; reservation?: DHCPReservation; close: () => void; saved: (c: DHCPConfig) => void }) {
  const api = useApi();
  const onConflict = useRefreshOnConflict(config.scope);
  const [etag] = useState(config.etag);
  const [draft, setDraft] = useState<DHCPReservation>(reservation ?? { mac: '', ip: '', hostname: '' });
  const [submitted, setSubmitted] = useState(false);
  const errors = { mac: macError(draft.mac), ip: ipError(draft.ip) ?? undefined, hostname: hostnameError(draft.hostname) };
  const mutation = useMutation({ onError: onConflict, mutationFn: () => api.dhcp.reserve(config.scope, { ...draft, mac: draft.mac.toLowerCase() }, etag), onSuccess: (next) => { saved(next); close(); } });
  return <FormDialog isOpen onClose={close} title={reservation ? 'Edit reservation' : 'Add reservation'}
    onSubmit={() => { setSubmitted(true); if (!Object.values(errors).some(Boolean)) mutation.mutate(); }}
    footer={<><Button label="Cancel" onClick={close} /><Button label="Save reservation" type="submit" variant="primary" isLoading={mutation.isPending} isDisabled={etag !== config.etag} /></>}>
    {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
    {etag !== config.etag ? <Text>This scope changed. Close and reopen this dialog to use the saved values.</Text> : null}
    <TextField label="MAC address" value={draft.mac} onChange={(mac) => setDraft({ ...draft, mac })} isDisabled={!!reservation} mono placeholder="02:00:00:00:00:10" error={submitted ? errors.mac : undefined} />
    <TextField label="Reserved address" value={draft.ip} onChange={(ip) => setDraft({ ...draft, ip })} mono placeholder="192.0.2.10" error={submitted ? errors.ip : undefined} help="Must be inside this subnet and available to this client." />
    <TextField label="Hostname" value={draft.hostname} onChange={(hostname) => setDraft({ ...draft, hostname })} placeholder="printer" error={submitted ? errors.hostname : undefined} help="Optional. Takes precedence over the client's hostname." />
  </FormDialog>;
}

function Reservations({ config, saved }: { config: DHCPConfig; saved: (v: DHCPConfig) => void }) {
  const api = useApi();
  const can = useCan();
  const onConflict = useRefreshOnConflict(config.scope);
  const [editing, setEditing] = useState<{ reservation?: DHCPReservation } | null>(null);
  const [removing, setRemoving] = useState<{ reservation: DHCPReservation; etag: string } | null>(null);
  const managed = config.management === 'api' && !config.envLocked.includes('reservations');
  const readOnly = config.managedBy ? managedText(config.managedBy) : 'Managed outside the dashboard';
  const mutation = useMutation({
    onError: onConflict,
    mutationFn: async () => { if (!removing) throw new Error('Choose a reservation'); return api.dhcp.removeReservation(config.scope, removing.reservation.mac, removing.etag); },
    onSuccess: (next) => { saved(next); setRemoving(null); },
  });
  type Row = DHCPReservation & Record<string, unknown>;
  const columns: TableColumn<Row>[] = [
    { key: 'mac', header: 'MAC address', width: proportional(2) },
    { key: 'ip', header: 'Reserved address', width: proportional(2) },
    { key: 'hostname', header: 'Hostname', width: proportional(2), renderCell: (r) => <Text>{r.hostname || '—'}</Text> },
    { key: 'actions', header: 'Actions', width: proportional(2), renderCell: (r) => <HStack gap={1}>
      <Button label={`Edit ${r.mac}`} variant="ghost" size="sm" isDisabled={!managed || !can('dhcp', 'write')} tooltip={!managed ? readOnly : !can('dhcp', 'write') ? 'Requires dhcp.write' : undefined} onClick={() => setEditing({ reservation: r })} />
      <Button label={`Remove ${r.mac}`} variant="ghost" size="sm" isDisabled={!managed || !can('dhcp', 'delete')} tooltip={!managed ? readOnly : !can('dhcp', 'delete') ? 'Requires dhcp.delete' : undefined} onClick={() => { mutation.reset(); setRemoving({ reservation: r, etag: config.etag }); }} />
    </HStack> },
  ];
  return <Section padding={0} variant="transparent"><VStack gap={4}>
    <HStack hAlign="between" wrap="wrap" gap={2} vAlign="center"><Heading level={2}>Reservations</Heading>
      <Button label="Add reservation" isDisabled={!managed || !can('dhcp', 'write')} tooltip={!managed ? readOnly : !can('dhcp', 'write') ? 'Requires dhcp.write' : undefined} onClick={() => setEditing({})} /></HStack>
    <Table<Row> aria-label="DHCP reservations" density="compact" idKey="mac" data={config.reservations} columns={columns} />
    {editing ? <ReservationDialog config={config} {...editing} close={() => setEditing(null)} saved={saved} /> : null}
    <FormDialog isOpen={!!removing} onClose={() => setRemoving(null)} title="Remove reservation"
      footer={<><Button label="Cancel" onClick={() => setRemoving(null)} /><Button label="Remove reservation" variant="destructive" isLoading={mutation.isPending} isDisabled={removing?.etag !== config.etag} onClick={() => mutation.mutate()} /></>}>
      {removing && removing.etag !== config.etag ? <Text>This scope changed. Close and reopen this dialog to use the saved values.</Text> : null}
      <Text>Remove the reservation for {removing?.reservation.mac}? Its active lease will remain until it expires or the client releases it.</Text>
      {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
    </FormDialog>
  </VStack></Section>;
}

function ImportDialog({ scope, close }: { scope: string; close: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [format, setFormat] = useState<'native' | 'dnsmasq'>('native');
  const [data, setData] = useState('');
  const tooLarge = new TextEncoder().encode(data).byteLength > 4 * 1024 * 1024;
  const mutation = useMutation({ mutationFn: () => api.dhcp.import(scope, format, data), onSuccess: () => void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'leases') }) });
  return <FormDialog isOpen onClose={close} title="Import leases" onSubmit={() => { if (data.trim() && !tooLarge) mutation.mutate(); }}
    footer={<><Button label={mutation.isSuccess ? 'Close' : 'Cancel'} onClick={close} /><Button label="Import leases" type="submit" variant="primary" isLoading={mutation.isPending} isDisabled={!data.trim() || tooLarge || mutation.isSuccess} /></>}>
    <Text type="supporting">Merge active leases without replacing an existing allocation or shortening its expiry. Stop the old DHCP service before cutover.</Text>
    <SelectField label="Lease format" value={format} options={[{ value: 'native', label: 'kudurru JSON' }, { value: 'dnsmasq', label: 'dnsmasq leases' }]} onChange={(v) => { setFormat(v as 'native' | 'dnsmasq'); mutation.reset(); }} />
    <TextAreaField label="Lease data" value={data} onChange={(v) => { setData(v); mutation.reset(); }} rows={8} mono hasSpellCheck={false}
      help="Paste the exported file contents. Maximum size: 4 MiB." error={tooLarge ? 'The import exceeds 4 MiB' : undefined} />
    {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
    {mutation.data ? <Banner status="success" title="Lease import complete" description={`${mutation.data.imported} imported, ${mutation.data.unchanged ?? 0} unchanged, ${mutation.data.skippedExpired} expired rows skipped.`} /> : null}
  </FormDialog>;
}

function Leases({ scope }: { scope: string }) {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const [search, setSearch] = useState('');
  const q = useDeferredValue(search);
  const [state, setState] = useState('');
  const [importing, setImporting] = useState(false);
  const leases = useInfiniteQuery({
    queryKey: qk.dhcp(scope, 'leases', q, state),
    queryFn: ({ pageParam, signal }) => api.dhcp.leases(scope, { q, state, limit: 100, ...(pageParam ? { continue: pageParam } : {}) }, signal),
    initialPageParam: '', getNextPageParam: (last) => last.continue || undefined,
    enabled: can('dhcpleases', 'read'), refetchInterval: 15_000,
  });
  const exporting = useMutation({ mutationFn: () => api.dhcp.export(scope), onSuccess: (result) => {
    const url = URL.createObjectURL(new Blob([JSON.stringify(result.document, null, 2) + '\n'], { type: 'application/json' }));
    const link = document.createElement('a');
    link.href = url; link.download = `dhcp-${scope}-leases.json`; link.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }, onError: (error) => notify.error(error) });
  type Row = DHCPLease & Record<string, unknown>;
  const columns: TableColumn<Row>[] = [
    { key: 'address', header: 'Address', width: proportional(2), renderCell: (l) => <Link href={`/clients/${encodeURIComponent(l.address)}`}>{l.address}</Link> },
    { key: 'mac', header: 'MAC address', width: proportional(2) },
    { key: 'hostname', header: 'Hostname', width: proportional(2), renderCell: (l) => <Text>{l.hostname || '—'}</Text> },
    { key: 'state', header: 'State', renderCell: (l) => <Text>{l.state === 'bound' ? 'Leased' : l.state === 'declined' ? 'Quarantined' : 'Offered'}</Text> },
    { key: 'expiresAt', header: 'Expires', width: proportional(2), renderCell: (l) => <Text>{formatRelative(l.expiresAt)}</Text> },
  ];
  return <Section padding={0} variant="transparent"><VStack gap={4}>
    <HStack hAlign="between" wrap="wrap" gap={2} vAlign="center"><Heading level={2}>Leases</Heading><HStack gap={2}>
      <Button label="Export leases" isDisabled={!can('dhcpleases', 'export')} tooltip={!can('dhcpleases', 'export') ? 'Requires dhcpleases.export' : undefined} isLoading={exporting.isPending} onClick={() => exporting.mutate()} />
      <Button label="Import leases" isDisabled={!can('dhcpleases', 'import')} tooltip={!can('dhcpleases', 'import') ? 'Requires dhcpleases.import' : undefined} onClick={() => setImporting(true)} />
    </HStack></HStack>
    {!can('dhcpleases', 'read') ? <Text type="supporting">Viewing lease inventory requires dhcpleases.read permission.</Text> : <>
      <HStack gap={3} wrap="wrap" vAlign="start">
        <TextField label="Find a lease" value={search} onChange={setSearch} placeholder="Address, MAC or hostname" />
        <SelectField label="Lease state" value={state} onChange={setState} options={[{ value: '', label: 'All states' }, { value: 'bound', label: 'Leased' }, { value: 'offered', label: 'Offered' }, { value: 'declined', label: 'Quarantined' }]} />
      </HStack>
      {leases.error ? <ErrorBanner error={leases.error} /> : null}
      {leases.isPending ? <Loading label="Loading leases" /> : null}
      {leases.data ? <Table<Row> aria-label="DHCP leases" density="compact" idKey="address" data={leases.data.pages.flatMap((p) => p.items)} columns={columns} /> : null}
      {leases.hasNextPage ? <Button label="Load more leases" isLoading={leases.isFetchingNextPage} onClick={() => void leases.fetchNextPage()} /> : null}
    </>}
    {importing ? <ImportDialog scope={scope} close={() => setImporting(false)} /> : null}
  </VStack></Section>;
}

export default function DHCPScopePage({ params }: PageProps) {
  const scope = params.scope ?? 'default';
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const setup = useDHCPSetup();
  const status = useQuery({ queryKey: qk.dhcp('status'), queryFn: api.dhcp.status, enabled: can('dhcp', 'read'), refetchInterval: 15_000 });
  const running = !!status.data?.available && status.data.scopes.some((s) => s.name === scope);
  const desired = setup.data?.scopes.find((s) => s.name === scope);
  const config = useQuery({ queryKey: qk.dhcp(scope, 'config'), queryFn: () => api.dhcp.config(scope), enabled: can('dhcp', 'read') && running, refetchInterval: 15_000 });
  const saved = (next: DHCPConfig) => {
    qc.setQueryData(qk.dhcp(scope, 'config'), next);
    void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'leases') });
    void qc.invalidateQueries({ queryKey: qk.dhcp('status') });
    void qc.invalidateQueries({ queryKey: qk.dhcp('setup') });
  };
  const c = config.data;
  const kube = isKube(status.data) || isKube(setup.data);
  const invalid = status.data?.invalidScopes?.[scope];
  const meta = kube ? status.data && dhcpServingText(status.data) : c && `Connected node: ${c.node}`;
  return <DHCPFrame>
    <PageHeader title={`DHCP: ${scope}`} back={{ href: '/dhcp', label: 'DHCP scopes' }} meta={meta ? <Text type="supporting">{meta}</Text> : undefined} />
    {!can('dhcp', 'read') ? <Text>Viewing this scope requires dhcp.read permission.</Text> : null}
    {config.error ? <ErrorBanner error={config.error} /> : null}
    {can('dhcp', 'read') && (setup.isPending || status.isPending || (running && config.isPending)) ? <Loading label="Loading DHCP scope" /> : null}
    {setup.error ? <ErrorBanner error={setup.error} /> : null}
    {status.error ? <ErrorBanner error={status.error} /> : null}
    {setup.data ? <DHCPSetupNotices setup={setup.data} /> : null}
    <DHCPFailoverStatus value={status.data?.failover} />
    {invalid ? <Banner status="warning" title="This scope is not served" description={invalid} /> : null}
    {setup.data && desired && status.data ? <DHCPSavedScope key={scope} setup={setup.data} scope={desired} running={running} /> : null}
    {!desired && setup.data && !running ? <Text>This scope is not in the saved setup. Return to DHCP scopes to create one.</Text> : null}
    {running && c ? <>
      <Heading level={2}>Running scope</Heading>
      {!kube && setup.data?.pendingScopes.includes(scope) ? <Banner status="info" title="This scope also has changes saved for restart"
        description="If you change running options or reservations below, re-save the pending setup before restarting so it uses the latest settings revision." /> : null}
      <MetadataList columns="multi">
        <MetadataListItem label="Interface">{c.topology.interface}</MetadataListItem>
        <MetadataListItem label="Server address">{c.topology.serverIP}</MetadataListItem>
        <MetadataListItem label="Subnet">{c.topology.subnet}</MetadataListItem>
        <MetadataListItem label="Clients">{c.topology.relayOnly ? 'Via trusted relay' : 'Direct LAN'}</MetadataListItem>
      </MetadataList>
      {c.topology.trustedRelays.length ? <Text type="supporting">Trusted relays: {c.topology.trustedRelays.map((r) => `${r.sourceIP} (gateway ${r.gatewayIP})`).join(', ')}</Text> : null}
      {c.managedBy ? setup.data?.scopeOwners?.[scope] ? null : <Banner status="info" title={managedText(c.managedBy)} description="Network options and reservations are read-only here." />
        : c.management === 'file' ? <Banner status="info" title="Managed by the configuration file" description="Network options and reservations are read-only here. Choose Manage in dashboard on the DHCP page to adopt this setup, then restart to enable dashboard editing." /> : null}
      {c.envLocked.length ? <Banner status="info" title="Some values are set by the environment" description={`Locked fields: ${c.envLocked.join(', ')}. Change those environment variables and restart the node.`} /> : null}
      <OptionsEditor key={scope} config={c} saved={saved} />
      <Reservations key={`${scope}-reservations`} config={c} saved={saved} />
      <Leases key={`${scope}-leases`} scope={scope} />
    </> : null}
  </DHCPFrame>;
}
