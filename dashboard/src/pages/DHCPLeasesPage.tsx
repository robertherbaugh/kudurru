// The DHCP client list: the lease inventory of one running scope, with
// search, a state filter, export, import and reserving a row's client.
// Filtering > Clients lists DNS clients instead; a lease's address links there.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/Stack';
import { Table, proportional, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useDeferredValue, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { DHCPConfig, DHCPLease } from '../api/types';
import { ReservationDialog } from '../components/DHCPReservationDialog';
import { managedText } from '../components/DHCPSetupFields';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { SelectField } from '../components/fields/SelectField';
import { TextAreaField, TextField } from '../components/fields/TextField';
import { PageHeader } from '../components/layout/Page';
import { formatRelative } from '../lib/format';
import { useSearchParams } from '../lib/router';
import { DHCPFrame, dhcpServingText } from './DHCPPage';

function ImportDialog({ scope, close }: { scope: string; close: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const [format, setFormat] = useState<'native' | 'dnsmasq'>('native');
  const [data, setData] = useState('');
  const tooLarge = new TextEncoder().encode(data).byteLength > 4 * 1024 * 1024;
  const mutation = useMutation({ mutationFn: () => api.dhcp.import(scope, format, data), onSuccess: () => void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'leases') }) });
  return <FormDialog isOpen onClose={close} title="Import leases" onSubmit={() => { if (data.trim() && !tooLarge) mutation.mutate(); }}
    footer={<><Button label={mutation.isSuccess ? 'Close' : 'Cancel'} onClick={close} /><Button label="Import leases" type="submit" variant="primary" isLoading={mutation.isPending} isDisabled={!data.trim() || tooLarge || mutation.isSuccess} /></>}>
    <Text type="supporting">Merge active leases into {scope} without replacing an existing allocation or shortening its expiry. Stop the old DHCP service before cutover.</Text>
    <SelectField label="Lease format" value={format} options={[{ value: 'native', label: 'kudurru JSON' }, { value: 'dnsmasq', label: 'dnsmasq leases' }]} onChange={(v) => { setFormat(v as 'native' | 'dnsmasq'); mutation.reset(); }} />
    <TextAreaField label="Lease data" value={data} onChange={(v) => { setData(v); mutation.reset(); }} rows={8} mono hasSpellCheck={false}
      help="Paste the exported file contents. Maximum size: 4 MiB." error={tooLarge ? 'The import exceeds 4 MiB' : undefined} />
    {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
    {mutation.data ? <Banner status="success" title="Lease import complete" description={`${mutation.data.imported} imported, ${mutation.data.unchanged ?? 0} unchanged, ${mutation.data.skippedExpired} expired rows skipped.`} /> : null}
  </FormDialog>;
}

type Row = DHCPLease & Record<string, unknown>;
const columns: TableColumn<Row>[] = [
  { key: 'address', header: 'Address', width: proportional(2), renderCell: (l) => <Link href={`/clients/${encodeURIComponent(l.address)}`}>{l.address}</Link> },
  { key: 'mac', header: 'MAC address', width: proportional(2) },
  { key: 'hostname', header: 'Hostname', width: proportional(2), renderCell: (l) => <Text>{l.hostname || '—'}</Text> },
  { key: 'state', header: 'State', renderCell: (l) => <Text>{l.state === 'bound' ? 'Leased' : l.state === 'declined' ? 'Quarantined' : 'Offered'}</Text> },
  { key: 'expiresAt', header: 'Expires', width: proportional(2), renderCell: (l) => <Text>{formatRelative(l.expiresAt)}</Text> },
];

// Why a lease cannot be reserved from this list, if it cannot.
function reserveBlocked(config: DHCPConfig | undefined, lease: DHCPLease, allowed: boolean) {
  if (!config) return 'Loading the scope';
  if (config.managedBy) return managedText(config.managedBy);
  if (config.management !== 'api' || config.envLocked.includes('reservations')) return 'Managed outside the dashboard';
  if (!allowed) return 'Requires dhcp.write';
  if (lease.state === 'declined') return 'A quarantined address cannot be reserved';
  const current = config.reservations.find((r) => r.mac === lease.mac);
  if (current?.ip === lease.address) return 'Already reserved for this client';
  return undefined;
}

function Leases({ scope, scopes, onScope }: { scope: string; scopes: string[]; onScope: (scope: string) => void }) {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const qc = useQueryClient();
  const [search, setSearch] = useState('');
  const q = useDeferredValue(search);
  const [state, setState] = useState('');
  const [importing, setImporting] = useState(false);
  const [reserving, setReserving] = useState<DHCPLease | null>(null);
  const config = useQuery({ queryKey: qk.dhcp(scope, 'config'), queryFn: () => api.dhcp.config(scope), enabled: can('dhcp', 'read') && can('dhcpleases', 'read'), refetchInterval: 15_000 });
  const saved = (next: DHCPConfig) => {
    qc.setQueryData(qk.dhcp(scope, 'config'), next);
    void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'leases') });
    void qc.invalidateQueries({ queryKey: qk.dhcp('status') });
    void qc.invalidateQueries({ queryKey: qk.dhcp('setup') });
  };
  const rowColumns: TableColumn<Row>[] = [...columns, { key: 'actions', header: 'Actions', width: proportional(2), renderCell: (l) => {
    const blocked = reserveBlocked(config.data, l, can('dhcp', 'write'));
    const current = config.data?.reservations.find((r) => r.mac === l.mac);
    return <Button label={current ? `Edit ${l.mac}` : `Reserve ${l.address}`} variant="ghost" size="sm" isDisabled={!!blocked} tooltip={blocked}
      onClick={() => setReserving(l)} />;
  } }];
  const reservingCurrent = reserving ? config.data?.reservations.find((r) => r.mac === reserving.mac) : undefined;
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
  return <>
    <HStack hAlign="between" wrap="wrap" gap={3} vAlign="end">
      <HStack gap={3} wrap="wrap" vAlign="start">
        <SelectField label="Scope" value={scope} onChange={onScope} options={scopes.map((s) => ({ value: s, label: s }))} />
        {can('dhcpleases', 'read') ? <>
          <TextField label="Find a lease" value={search} onChange={setSearch} placeholder="Address, MAC or hostname" />
          <SelectField label="Lease state" value={state} onChange={setState} options={[{ value: '', label: 'All states' }, { value: 'bound', label: 'Leased' }, { value: 'offered', label: 'Offered' }, { value: 'declined', label: 'Quarantined' }]} />
        </> : null}
      </HStack>
      <HStack gap={2}>
        <Button label="Export leases" isDisabled={!can('dhcpleases', 'export')} tooltip={!can('dhcpleases', 'export') ? 'Requires dhcpleases.export' : undefined} isLoading={exporting.isPending} onClick={() => exporting.mutate()} />
        <Button label="Import leases" isDisabled={!can('dhcpleases', 'import')} tooltip={!can('dhcpleases', 'import') ? 'Requires dhcpleases.import' : undefined} onClick={() => setImporting(true)} />
      </HStack>
    </HStack>
    {!can('dhcpleases', 'read') ? <EmptyState title="Lease inventory is restricted" description="Viewing lease inventory requires dhcpleases.read permission." /> : <>
      {leases.error ? <ErrorBanner error={leases.error} /> : null}
      {leases.isPending ? <Loading label="Loading leases" /> : null}
      {leases.data ? <Table<Row> aria-label="DHCP leases" density="compact" idKey="address" data={leases.data.pages.flatMap((p) => p.items)} columns={can('dhcp', 'read') ? rowColumns : columns} /> : null}
      {leases.hasNextPage ? <Button label="Load more leases" isLoading={leases.isFetchingNextPage} onClick={() => void leases.fetchNextPage()} /> : null}
    </>}
    {importing ? <ImportDialog scope={scope} close={() => setImporting(false)} /> : null}
    {reserving && config.data ? <ReservationDialog config={config.data} close={() => setReserving(null)} saved={saved}
      {...(reservingCurrent ? { reservation: reservingCurrent } : { initial: { mac: reserving.mac, ip: reserving.address, hostname: reserving.hostname } })} /> : null}
  </>;
}

export default function DHCPLeasesPage() {
  const api = useApi();
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const allowed = can('dhcp', 'read');
  const status = useQuery({ queryKey: qk.dhcp('status'), queryFn: api.dhcp.status, enabled: allowed, refetchInterval: 15_000 });
  // Leases live in a running scope's allocator, so only running scopes are listed.
  const scopes = status.data?.available ? status.data.scopes.map((s) => s.name) : [];
  const requested = sp.get('scope') ?? '';
  const scope = scopes.includes(requested) ? requested : scopes[0];
  return <DHCPFrame>
    <PageHeader title="DHCP leases" description="Devices that hold or were offered an address by a running scope."
      meta={status.data ? <Text type="supporting">{dhcpServingText(status.data)}</Text> : undefined} />
    {!allowed ? <EmptyState title="DHCP access is restricted" description="Viewing DHCP requires dhcp.read permission." /> : null}
    {status.error ? <ErrorBanner error={status.error} /> : null}
    {allowed && status.isPending ? <Loading label="Loading DHCP scopes" /> : null}
    {status.data && requested && scope && requested !== scope ? <Banner status="warning" title={`Scope ${requested} is not running`} description={`Showing leases of ${scope} instead.`} /> : null}
    {scope ? <Leases key={scope} scope={scope} scopes={scopes} onScope={(next) => setParams({ scope: next })} />
      : status.data ? <EmptyState title="No DHCP scope is running" description="Leases appear here once a scope from DHCP scopes is running." /> : null}
  </DHCPFrame>;
}
