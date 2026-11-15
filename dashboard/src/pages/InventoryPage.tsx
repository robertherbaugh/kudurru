// The client inventory (GET /inventory, D397): every device the inventory
// knows, whether or not it sent a DNS query. Devices come from inventory
// integrations (UniFi, a webhook peer) and from the answering node's bound
// DHCP leases; each lists its MACs, names, tags, observation and expiry, and
// what each of its addresses means for DNS identity now. Filtering > Clients
// is the query-log view; a device links there for every address the query
// log knows, and to the Integration or DHCP scope that reported it. Read
// only: it needs inventory.read and changes nothing, so a follower and a
// static host show it as any node does.
import { Banner } from '@astryxdesign/core/Banner';
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Code } from '@astryxdesign/core/Code';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { useDeferredValue, useMemo } from 'react';
import { useApi, useCan, useClusterInfo, useClusterRole } from '../api/context';
import type { InventoryAddress, InventoryDevice, InventoryQuery } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { SelectField } from '../components/fields/SelectField';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader } from '../components/layout/Page';
import { formatDateTime, formatRelative } from '../lib/format';
import { integrationHref } from '../lib/integrations';
import { ADDRESS_STATE_TEXT, deviceName, dhcpScope, integrationsBySource, inventoryUnavailable, sourceRef, sqliteClusterMember } from '../lib/inventory';
import { clientHref } from '../lib/policies';
import { useSearchParams } from '../lib/router';
import { useCanReadIntegrations, useIntegrationObjects, useIntegrationSummaries } from '../lib/useIntegrations';

export const INVENTORY_KEY = ['inventory'] as const;
const PAGE = 200;
const POLL_MS = 30_000;

type Row = InventoryDevice & { key: string } & Record<string, unknown>;

const KIND_OPTIONS = [
  { value: '', label: 'All devices' },
  { value: 'inventory', label: 'Integration devices', help: 'Devices an inventory integration (UniFi, a webhook peer) reports.' },
  { value: 'dhcp', label: 'DHCP leases', help: 'Bound leases of this node’s DHCP server.' },
];
const STALE_OPTIONS = [
  { value: '', label: 'Live and stale' },
  { value: 'false', label: 'Live only' },
  { value: 'true', label: 'Stale only', help: 'The observation or lease has expired; the device no longer identifies queries.' },
];

function AddressCell({ a, known }: { a: InventoryAddress; known: ReadonlySet<string> }) {
  const state = ADDRESS_STATE_TEXT[a.state];
  const owner = a.state === 'superseded' && a.ownerSource ? ` by ${a.ownerId ?? 'a device'} of ${a.ownerSource}` : '';
  return (
    <HStack gap={1} vAlign="center" wrap="wrap">
      {known.has(a.ip) ? (
        <Tooltip content="Open the client page of this address">
          <Link href={clientHref(a.ip)}>
            <Code color="inherit" size="inherit">{a.ip}</Code>
          </Link>
        </Tooltip>
      ) : (
        <Code>{a.ip}</Code>
      )}
      <Tooltip content={state.help}>
        <Text type="supporting" data-state={a.state}>
          {state.label}
          {owner}
        </Text>
      </Tooltip>
    </HStack>
  );
}

export default function InventoryPage() {
  const api = useApi();
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const allowed = can('inventory', 'read');
  const filter: InventoryQuery = {
    source: sp.get('source') ?? '',
    kind: (sp.get('kind') ?? '') as InventoryQuery['kind'],
    stale: (sp.get('stale') ?? '') as InventoryQuery['stale'],
  };
  const search = sp.get('q') ?? '';
  const q = useDeferredValue(search.trim());

  const devices = useInfiniteQuery({
    queryKey: [...INVENTORY_KEY, filter.source, filter.kind, filter.stale, q],
    queryFn: ({ pageParam, signal }) => api.inventory.list({ ...filter, q, limit: PAGE, ...(pageParam ? { continue: pageParam } : {}) }, signal),
    initialPageParam: '',
    getNextPageParam: (last) => last.continue || undefined,
    enabled: allowed,
    refetchInterval: POLL_MS,
  });

  // The Integration that reported a device: each incarnation's summary names
  // the inventory source its devices carry (integrations.read only).
  const readsIntegrations = useCanReadIntegrations();
  const objects = useIntegrationObjects();
  const namespaces = useMemo(() => [...new Set((objects.data?.items ?? []).map((o) => o.metadata.namespace ?? 'default'))].sort(), [objects.data]);
  const summaries = useIntegrationSummaries(readsIntegrations ? namespaces : []);
  const bySource = useMemo(() => integrationsBySource(summaries.byKey.values()), [summaries.byKey]);

  // Addresses the query log knows have a client page with traffic.
  const readsClients = can('clients', 'read');
  const clients = useQuery({ queryKey: ['clients', 'inventory-links'], queryFn: () => api.clients.list({ scope: 'cluster' }, { window: '24h' }), enabled: allowed && readsClients, refetchInterval: 60_000 });
  const known = useMemo(() => new Set((clients.data?.items ?? []).map((c) => c.key)), [clients.data]);

  const cluster = useClusterInfo();
  const role = useClusterRole();
  const pages = devices.data?.pages ?? [];
  const first = pages[0];
  const nodeLocal = sqliteClusterMember(cluster.data, role, first?.nodeLocalLeases);
  const rows: Row[] = pages.flatMap((p) => p.items).map((d) => ({ ...d, key: `${d.source}\u0000${d.id}` }));

  const sourceOptions = useMemo(() => {
    const opts = [{ value: '', label: 'All sources' }];
    const seen = new Set<string>();
    for (const [source, s] of bySource) {
      seen.add(source);
      opts.push({ value: source, label: `Integration ${s.namespace === 'default' ? s.name : `${s.namespace}/${s.name}`}` });
    }
    for (const d of rows) {
      if (seen.has(d.source)) continue;
      seen.add(d.source);
      opts.push({ value: d.source, label: sourceRef(d.source, bySource, integrationHref).label });
    }
    if (filter.source && !seen.has(filter.source)) opts.push({ value: filter.source, label: sourceRef(filter.source, bySource, integrationHref).label });
    return opts;
  }, [bySource, rows, filter.source]);

  const columns: TableColumn<Row>[] = [
    {
      key: 'device',
      header: 'Device',
      width: proportional(2.5, { minWidth: 180 }),
      renderCell: (d) => {
        const page = d.addresses.find((a) => a.state === 'assigned' && known.has(a.ip)) ?? d.addresses.find((a) => known.has(a.ip));
        const name = deviceName(d);
        return (
          <VStack gap={0.5}>
            {page ? (
              <Link href={clientHref(page.ip)} hasUnderline>
                {name}
              </Link>
            ) : (
              <Text>{name}</Text>
            )}
            {d.hostname && d.hostname !== name ? <Text type="supporting">hostname {d.hostname}</Text> : null}
            {d.kind === 'inventory' && d.id !== name ? <Text type="supporting">ID {d.id}</Text> : null}
            {d.tags.length ? <Text type="supporting">tags {d.tags.join(', ')}</Text> : null}
          </VStack>
        );
      },
    },
    {
      key: 'macs',
      header: 'MAC addresses',
      width: proportional(1.5, { minWidth: 150 }),
      renderCell: (d) =>
        d.macs.length ? (
          <VStack gap={0}>
            {d.macs.map((m) => (
              <Code key={m}>{m}</Code>
            ))}
          </VStack>
        ) : (
          <Text color="secondary">none reported</Text>
        ),
    },
    {
      key: 'addresses',
      header: 'Addresses and state',
      width: proportional(2.5, { minWidth: 220 }),
      renderCell: (d) =>
        d.addresses.length ? (
          <VStack gap={0.5}>
            {d.addresses.map((a) => (
              <AddressCell key={a.ip} a={a} known={known} />
            ))}
          </VStack>
        ) : (
          <Text color="secondary">no address</Text>
        ),
    },
    {
      key: 'source',
      header: 'Source',
      width: proportional(1.5, { minWidth: 150 }),
      renderCell: (d) => {
        const ref = sourceRef(d.source, bySource, integrationHref);
        return ref.href ? (
          <Link href={ref.href}>{ref.label}</Link>
        ) : (
          <Tooltip content={readsIntegrations ? 'No Integration of this cluster carries this source' : 'Reading which Integration reported it needs integrations.read'}>
            <Text>
              <Code color="inherit" size="inherit">
                {ref.label}
              </Code>
            </Text>
          </Tooltip>
        );
      },
    },
    {
      key: 'seen',
      header: 'Seen and expiry',
      width: proportional(1.5, { minWidth: 160 }),
      renderCell: (d) => (
        <VStack gap={0}>
          {d.observedAt ? (
            <Tooltip content={formatDateTime(d.observedAt)}>
              <Text>seen {formatRelative(d.observedAt)}</Text>
            </Tooltip>
          ) : (
            <Text>DHCP lease</Text>
          )}
          <Tooltip content={formatDateTime(d.expiresAt)}>
            <Text type="supporting">
              {d.stale ? 'expired' : 'expires'} {formatRelative(d.expiresAt)}
            </Text>
          </Tooltip>
        </VStack>
      ),
    },
    {
      key: 'stale',
      header: 'State',
      width: proportional(1, { minWidth: 100 }),
      // Only the exception is a badge: a stale device identifies nobody.
      renderCell: (d) => (d.stale ? <Badge variant="warning" label="Stale" /> : <Text>Live</Text>),
    },
  ];

  const unavailable = inventoryUnavailable(devices.error);
  const filtered = !!(filter.source || filter.kind || filter.stale || q);
  const leases = first?.items.some((d) => dhcpScope(d.source) !== undefined) || filter.kind === 'dhcp';

  return (
    <Page>
      <PageHeader
        back={{ href: '/clients', label: 'Clients' }}
        title="Inventory"
        description="Every device the client inventory knows, whether or not it has sent a query: devices reported by inventory integrations and this node’s DHCP leases. A lease or integration MAC is what ClientGroup mac matchers match."
        meta={
          first ? (
            <Text type="supporting" data-testid="inventory-meta">
              Answered by node {first.node} at {formatDateTime(first.readAt)}
              {first.generation === 0 ? ' · no inventory source has reported yet' : ''}
            </Text>
          ) : undefined
        }
      />
      {!allowed ? (
        <EmptyState title="The inventory is restricted" description="Viewing the client inventory requires the inventory.read permission (auditor, operator and admin roles). Filtering > Clients lists the clients the query log has seen." />
      ) : (
        <>
          {nodeLocal ? (
            <Banner
              status="info"
              title="DHCP leases are this node’s own"
              description={`This node is a member of a SQLite cluster. Lease devices and lease MACs come from ${first ? `node ${first.node}` : 'the node that answers'} only: a lease MAC selects a mac matcher only for queries this node answers, so the same client can get another policy on another node. For cluster-wide MAC matching use an inventory integration, or ip and cidr matchers.`}
            />
          ) : null}
          <HStack gap={3} wrap="wrap" vAlign="start">
            <TextField label="Find a device" startIcon="search" placeholder="Name, hostname, ID, MAC or address" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
            <SelectField label="Source" value={filter.source ?? ''} options={sourceOptions} onChange={(v) => setParams({ source: v || null })} />
            <SelectField label="Kind" value={filter.kind ?? ''} options={KIND_OPTIONS} onChange={(v) => setParams({ kind: v || null })} />
            <SelectField label="Staleness" value={filter.stale ?? ''} options={STALE_OPTIONS} onChange={(v) => setParams({ stale: v || null })} />
          </HStack>
          {unavailable === 'unavailable' ? (
            <Banner status="warning" title="The serving inventory is unavailable" description="This node has no serving inventory to read right now, for example while it starts or before its first serving generation. The page asks again every 30 seconds." />
          ) : unavailable === 'unmounted' ? (
            <Banner status="info" title="This server has no client inventory" description="The API answered that the client inventory is not mounted on this server, so there are no inventory devices or DHCP leases to list." />
          ) : devices.error ? (
            <ErrorBanner error={devices.error} />
          ) : null}
          {devices.isPending ? <Loading label="Loading the inventory" /> : null}
          {devices.data ? (
            <Card padding={0}>
              <Table<Row>
                aria-label="Inventory devices"
                density="compact"
                hasHover
                idKey="key"
                data={rows}
                columns={columns}
                emptyState={
                  <Text color="secondary">
                    {filtered ? 'No device matches these filters.' : 'The inventory is empty: no inventory integration has reported a device and this node holds no DHCP lease.'}
                  </Text>
                }
              />
            </Card>
          ) : null}
          {devices.hasNextPage ? <Button label="Load more devices" isLoading={devices.isFetchingNextPage} onClick={() => void devices.fetchNextPage()} /> : null}
          {leases && !nodeLocal && devices.data ? <Text type="supporting">DHCP leases are those of node {first?.node ?? 'answering'}.</Text> : null}
        </>
      )}
    </Page>
  );
}
