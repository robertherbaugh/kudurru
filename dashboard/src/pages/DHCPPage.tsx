// Adapted from Astryx's table-page template: facts above a flat row collection.
import { Banner } from '@astryxdesign/core/Banner';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Layout, LayoutContent } from '@astryxdesign/core/Layout';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { Table, proportional, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { DHCPScopeSummary, DHCPStatus } from '../api/types';
import { DHCPSetupControls, useDHCPSetup } from '../components/DHCPSetup';
import { isKube } from '../components/DHCPSetupFields';
import { DHCPFailoverStatus } from '../components/DHCPFailover';
import { ErrorBanner, Loading } from '../components/Feedback';
import { PageHeader } from '../components/layout/Page';

export function DHCPFrame({ children }: { children: ReactNode }) {
  return <Layout content={<LayoutContent padding={0}><VStack padding={4} gap={8}>{children}</VStack></LayoutContent>} />;
}

// Header facts: the node this dashboard talks to, or in Kubernetes mode the
// replica that answers DHCP and the LoadBalancer address relays send to.
export function dhcpServingText(status: DHCPStatus, node = status.node) {
  if (!isKube(status)) return `Connected node: ${node}`;
  return [status.servingNode ? `Serving replica: ${status.servingNode}` : 'No replica is serving', status.serverAddress ? `Relays send to ${status.serverAddress}` : ''].filter(Boolean).join(' · ');
}

type ScopeRow = DHCPScopeSummary & { activation: string } & Record<string, unknown>;
const columns: TableColumn<ScopeRow>[] = [
  { key: 'name', header: 'Scope', width: proportional(2), renderCell: (s) => <Link href={`/dhcp/${encodeURIComponent(s.name)}`}>{s.name}</Link> },
  { key: 'subnet', header: 'Subnet', width: proportional(2) },
  { key: 'interface', header: 'Interface' },
  { key: 'serverIP', header: 'Server address', width: proportional(2) },
  { key: 'relayOnly', header: 'Clients', renderCell: (s) => <Text>{s.relayOnly ? 'Via relay' : 'Direct LAN'}</Text> },
  { key: 'activation', header: 'State', width: proportional(2) },
];

export default function DHCPPage() {
  const api = useApi();
  const can = useCan();
  const allowed = can('dhcp', 'read');
  const status = useQuery({ queryKey: qk.dhcp('status'), queryFn: api.dhcp.status, enabled: allowed, refetchInterval: 15_000 });
  const setup = useDHCPSetup();
  const kube = isKube(status.data) || isKube(setup.data);
  const invalid = status.data?.invalidScopes ?? {};
  const active = status.data?.available ? status.data.scopes : [];
  const activation = (name: string) => kube
    ? invalid[name] ? 'Not served' : setup.data?.pendingScopes.includes(name) ? 'Applying' : active.some((a) => a.name === name) ? 'Running' : 'Saved, not running'
    : setup.data?.pendingScopes.includes(name) ? 'Changes pending' : active.some((a) => a.name === name) ? 'Running' : 'Saved, not running';
  const rows: ScopeRow[] = (setup.data?.scopes ?? []).map((s) => ({ ...s, management: setup.data?.managedBy ?? 'api', revision: '', activation: activation(s.name) }));
  for (const s of active) if (!rows.some((row) => row.name === s.name)) rows.push({ ...s, activation: setup.data ? 'Removal pending' : 'Running' });
  return <DHCPFrame>
    <PageHeader title="DHCP" description="IPv4 address allocation, reservations and local names."
      meta={status.data ? <Text type="supporting">{dhcpServingText(status.data)}</Text> : undefined} />
    {!allowed ? <EmptyState title="DHCP access is restricted" description="Viewing DHCP requires dhcp.read permission." /> : null}
    {status.error ? <ErrorBanner error={status.error} /> : null}
    {allowed && status.isPending ? <Loading label="Loading DHCP scopes" /> : null}
    {setup.error ? <ErrorBanner error={setup.error} /> : null}
    <DHCPFailoverStatus value={status.data?.failover} />
    {setup.data ? <DHCPSetupControls setup={setup.data} /> : null}
    {!kube && status.data && !status.data.enabled ? <Banner status="info" title="DHCP is disabled on this node"
      description="Create or edit saved scopes below, enable DHCP, then restart this node to activate it." /> : null}
    {status.data?.enabled && !status.data.available ? <Banner status="warning" title="DHCP is unavailable"
      description={kube ? 'The DHCP service is not running on this replica. Check the pod logs.' : 'The configured DHCP service is not running on this node. Check its startup logs.'} /> : null}
    {Object.keys(invalid).length ? <Banner status="warning" title="Some scopes are not served" collapsible={false}>
      <VStack gap={1}>{Object.entries(invalid).map(([name, reason]) => <Text key={name}>{name}: {reason}</Text>)}</VStack>
    </Banner> : null}
    {rows.length ? <Table<ScopeRow> aria-label="DHCP scopes" density="compact" idKey="name" data={rows} columns={columns} />
      : setup.data ? <EmptyState title="No DHCP scopes yet" description="Choose New scope to configure an address pool for your network." /> : null}
  </DHCPFrame>;
}
