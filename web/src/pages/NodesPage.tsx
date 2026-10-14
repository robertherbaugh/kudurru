// Nodes (§10 principles 9 and 13): readiness, zonecache state, version,
// leader and snapshot age per node (GET /nodes), the cluster summary
// (GET /cluster: store driver, epoch, leader), the edge supervisors
// (GET /edge: dnsdist and keepalived state, dnsdist backends) and the
// health components of this node on demand (GET /nodes/{name}/health).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Grid } from '@astryxdesign/core/Grid';
import { List, ListItem } from '@astryxdesign/core/List';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { StatusDot, type StatusDotVariant } from '@astryxdesign/core/StatusDot';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useQuery } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { qk, useApi } from '../api/context';
import { useNodes } from '../api/hooks';
import type { EdgeComponentState, HealthComponent, NodeInfo } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatRelative } from '../lib/format';
import { readiness } from '../lib/nodes';

type NodeRow = NodeInfo & Record<string, unknown>;
type ComponentRow = HealthComponent & { name: string } & Record<string, unknown>;

/** A status dot with its words next to it (and an optional note after them). */
function DotLabel({ variant, label, children }: { variant: StatusDotVariant; label: string; children?: ReactNode }) {
  return (
    <HStack gap={1} vAlign="center" wrap="wrap">
      <StatusDot variant={variant} label={label} />
      <Text type="inherit">{label}</Text>
      {children}
    </HStack>
  );
}

const componentColumns: TableColumn<ComponentRow>[] = [
  { key: 'name', header: 'Component', width: proportional(1, { minWidth: 120 }), renderCell: (c) => <Text weight="semibold">{c.name}</Text> },
  {
    key: 'ready',
    header: 'State',
    width: proportional(3, { minWidth: 160 }),
    renderCell: (c) => (
      <DotLabel variant={c.ready ? 'success' : 'error'} label={c.ready ? 'ready' : 'not ready'}>
        {c.message ? <Text color="secondary">{c.message}</Text> : null}
      </DotLabel>
    ),
  },
  {
    key: 'since',
    header: 'Since',
    width: pixel(140),
    renderCell: (c) =>
      c.since ? (
        <Tooltip content={c.since}>
          <Text size="sm">{formatRelative(c.since)}</Text>
        </Tooltip>
      ) : (
        <Text size="sm">–</Text>
      ),
  },
];

function Health({ name }: { name: string }) {
  const api = useApi();
  const q = useQuery({ queryKey: ['nodes', name, 'health'], queryFn: () => api.cluster.health(name) });
  if (q.error) return <ErrorBanner error={q.error} />;
  if (!q.data) return <Loading label={`Checking ${name}`} />;
  const h = q.data.health;
  const components: ComponentRow[] = Object.entries(h.components ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([c, v]) => ({ ...v, name: c }));
  return (
    <VStack gap={2}>
      <HStack gap={2} vAlign="center" wrap="wrap">
        <StatusDot variant={q.data.ready ? 'success' : 'error'} label={q.data.ready ? 'ready' : 'not ready'} />
        <Text weight="semibold">{h.status ?? (q.data.ready ? 'ready' : 'not ready')}</Text>
        {h.version ? <Text type="supporting">version {h.version}</Text> : null}
      </HStack>
      {h.unready?.length ? (
        <Text type="supporting" color="primary" as="p">
          Waiting for: {h.unready.join(', ')}
        </Text>
      ) : null}
      {components.length ? (
        <Table<ComponentRow> aria-label={`Health components of ${name}`} density="compact" idKey="name" data={components} columns={componentColumns} />
      ) : (
        <Text color="secondary" as="p">
          The node reported no components.
        </Text>
      )}
    </VStack>
  );
}

function EdgeRow({ label, c, off }: { label: string; c: EdgeComponentState | undefined; off: string }) {
  if (!c) return null;
  return (
    <MetadataListItem label={label}>
      {c.enabled ? (
        <DotLabel variant={c.state === 'degraded' ? 'warning' : c.state && /fail|error|down|fault|unavailable/i.test(c.state) ? 'error' : c.state === 'unknown' ? 'neutral' : 'success'} label={c.state ?? 'enabled'}>
          {c.message ? <Text type="supporting">{c.message}</Text> : null}
        </DotLabel>
      ) : (
        <Text color="secondary">{off}</Text>
      )}
    </MetadataListItem>
  );
}

export default function NodesPage() {
  const api = useApi();
  const nodes = useNodes();
  const cluster = useQuery({ queryKey: qk.cluster, queryFn: () => api.cluster.info() });
  const edge = useQuery({ queryKey: ['edge'], queryFn: () => api.cluster.edge(), refetchInterval: 10_000 });
  const [open, setOpen] = useState<string | null>(null);
  const c = cluster.data;
  const columns: TableColumn<NodeRow>[] = [
    {
      key: 'name',
      header: 'Node',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (n) => (
        <VStack gap={0.5}>
          <HStack gap={1} vAlign="center" wrap="wrap">
            <Text weight="semibold">{n.name}</Text>
            {n.self ? <Badge variant="neutral" label="this node" /> : null}
          </HStack>
          {n.address ? (
            <Text type="code" size="sm" color="secondary">
              {n.address}
            </Text>
          ) : null}
        </VStack>
      ),
    },
    {
      key: 'ready',
      header: 'Ready',
      width: proportional(2, { minWidth: 140 }),
      renderCell: (n) => {
        const r = readiness(n);
        return (
          <VStack gap={0.5}>
            <DotLabel variant={r.variant} label={r.label} />
            {n.unready?.length ? <Text type="supporting">waiting for {n.unready.join(', ')}</Text> : null}
            {n.error ? <Text type="supporting">{n.error}</Text> : null}
          </VStack>
        );
      },
    },
    { key: 'state', header: 'Zones', width: proportional(1, { minWidth: 72 }), renderCell: (n) => <Text size="sm">{n.state ?? '–'}</Text> },
    {
      key: 'version',
      header: 'Version',
      width: proportional(1, { minWidth: 110 }),
      renderCell: (n) => (
        <Text type="code" size="sm">
          {n.version ?? '–'}
        </Text>
      ),
    },
    {
      key: 'leader',
      header: 'Role',
      width: proportional(1, { minWidth: 90 }),
      renderCell: (n) => (n.leader ? <Badge variant="blue" label="leader" /> : <Text color="secondary">follower</Text>),
    },
    { key: 'snapshotAge', header: 'Snapshot age', width: proportional(1, { minWidth: 100 }), renderCell: (n) => <Text>{n.snapshotAge ?? '–'}</Text> },
    {
      key: 'health',
      header: <VisuallyHidden>Health</VisuallyHidden>,
      width: pixel(140),
      renderCell: (n) => (
        <Button
          label={open === n.name ? 'Hide health' : 'Health'}
          size="sm"
          variant="ghost"
          isDisabled={!n.self}
          tooltip={n.self ? undefined : `Ask ${n.name} directly; other nodes' health needs scatter-gather`}
          onClick={() => setOpen(open === n.name ? null : n.name)}
        />
      ),
    },
  ];
  return (
    <Page>
      <PageHeader
        title="Nodes"
        meta={
          c ? (
            <>
              <Badge variant="neutral" label={`store ${c.driver}`} />
              <Text type="supporting" color="primary">
                leader {c.leader || 'none'}
              </Text>
              {c.readOnly ? <Badge variant="warning" label="read-only" /> : null}
            </>
          ) : null
        }
      />
      {nodes.error ? <ErrorBanner error={nodes.error} /> : null}
      {nodes.isLoading ? <Loading /> : null}
      {nodes.data ? (
        <Card>
          <Table<NodeRow> aria-label="Nodes" density="compact" idKey="name" data={nodes.data.items} columns={columns} />
        </Card>
      ) : null}
      {open ? (
        <Panel title={`Health of ${open}`} headingId="health-title">
          <Health name={open} />
        </Panel>
      ) : null}
      <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
        <Panel title="Cluster" headingId="cluster-title">
          {cluster.error ? <ErrorBanner error={cluster.error} /> : null}
          {c ? (
            <MetadataList columns="multi">
              <MetadataListItem label="Store">{c.driver}</MetadataListItem>
              <MetadataListItem label="Leader">{c.leader || 'none'}</MetadataListItem>
              <MetadataListItem label="Nodes">{c.nodes}</MetadataListItem>
              <MetadataListItem label="Epoch">
                <Text type="code">{c.epoch}</Text>
              </MetadataListItem>
              <MetadataListItem label="Object writes">{c.readOnly ? 'refused (objects are file-managed on every node)' : 'accepted'}</MetadataListItem>
              {c.clusterID ? (
                <MetadataListItem label="Cluster ID">
                  <Text type="code" size="sm">
                    {c.clusterID}
                  </Text>
                </MetadataListItem>
              ) : null}
            </MetadataList>
          ) : null}
        </Panel>
        <Panel title="Edge" headingId="edge-title">
          {edge.error ? <ErrorBanner error={edge.error} /> : null}
          {edge.data ? (
            <>
              <MetadataList columns="multi">
                <EdgeRow label="dnsdist" c={edge.data.edge} off="not configured" />
                <EdgeRow label="VIP (keepalived)" c={edge.data.vip} off="not configured" />
              </MetadataList>
              {edge.data.supervisors?.length ? (
                <List density="compact" aria-label="Edge supervisors">
                  {edge.data.supervisors.map((s) => (
                    <ListItem key={s.node} label={s.node} description={`dnsdist: ${s.edge.state ?? 'not configured'}${s.vip.enabled ? `; keepalived: ${s.vip.state ?? 'unknown'}` : ''}`} />
                  ))}
                </List>
              ) : null}
              {edge.data.vips?.length ? (
                <List density="compact" aria-label="VIP states">
                  {edge.data.vips.map((v) => (
                    <ListItem key={`${v.name}:${v.node}`} label={`${v.name} on ${v.node}`} description={v.address}
                      endContent={<DotLabel variant={v.state === 'MASTER' ? 'success' : v.state === 'BACKUP' ? 'neutral' : v.state === 'unknown' ? 'warning' : 'error'} label={v.state} />} />
                  ))}
                </List>
              ) : null}
              {edge.data.backends.length ? (
                <List density="compact" aria-label="dnsdist backends">
                  {edge.data.backends.map((b) => (
                    <ListItem
                      key={`${b.node ?? ''}:${b.name}:${b.address}`}
                      label={b.name}
                      description={b.node ? `via ${b.node}` : undefined}
                      startContent={<StatusDot variant={b.up ? 'success' : 'error'} label={b.up ? 'up' : 'down'} />}
                      endContent={
                        <Text type="code" size="sm" color="secondary">
                          {b.address}
                        </Text>
                      }
                    />
                  ))}
                </List>
              ) : (
                <Text type="supporting" as="p">
                  No dnsdist backends reported.
                </Text>
              )}
            </>
          ) : null}
        </Panel>
      </Grid>
    </Page>
  );
}
