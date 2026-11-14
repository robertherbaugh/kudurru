// This node's managed root trust anchors (GET /cluster/anchors, D369) on the
// Nodes page: the accepted owner, its keys, the anchor workers, the last pull
// from the primary and, for every state that needs an operator, the
// documented action (lib/cluster.ts anchorAdvice; docs/deploy/sqlite-join.md).
// The status is diagnostic: it grants nothing and starts nothing.
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useApi } from '../../api/context';
import type { RootAnchorStatus } from '../../api/types';
import { anchorAdvice, CLUSTER_DOCS } from '../../lib/cluster';
import { formatDateTime } from '../../lib/format';
import { ErrorBanner, Loading } from '../Feedback';
import { Panel } from '../layout/Page';
import { clusterKeys } from '../wizards/ClusterWizards';

type KeyRow = NonNullable<RootAnchorStatus['owner']['keys']>[number] & Record<string, unknown>;

const keyColumns: TableColumn<KeyRow>[] = [
  { key: 'keyTag', header: 'Key tag', width: pixel(96), renderCell: (k) => <Text type="code">{k.keyTag}</Text> },
  { key: 'algorithm', header: 'Algorithm', width: pixel(96), renderCell: (k) => <Text>{k.algorithm}</Text> },
  { key: 'status', header: 'State', width: proportional(1, { minWidth: 100 }), renderCell: (k) => <Text>{k.status}</Text> },
  { key: 'firstSeen', header: 'First seen', width: proportional(1, { minWidth: 160 }), renderCell: (k) => <Text size="sm">{formatDateTime(k.firstSeen)}</Text> },
];

function worker(w: RootAnchorStatus['updater']): string {
  return `${w.state}${w.error ? ` (${w.error})` : w.lastError ? ` (last error ${w.lastError})` : ''}`;
}

export function RootAnchors() {
  const api = useApi();
  const q = useQuery({ queryKey: clusterKeys.anchors, queryFn: () => api.cluster.anchors(), retry: false, refetchInterval: 30_000 });
  const s = q.data;
  const advice = s ? anchorAdvice(s) : [];
  return (
    <Panel
      title="Root trust anchors"
      headingId="anchors-title"
      description="This node's root trust-anchor state (RFC 5011). Every member reports its own: open the dashboard of a member to see its state. A fenced or conflicted member closes its API; read its kudurru_root_anchor_* metrics, or stop it and run `kudurru anchors status --offline`."
    >
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading label="Reading the trust anchors" /> : null}
      {s ? (
        <VStack gap={3}>
          {advice.length ? (
            <List aria-label="Trust-anchor states that need you" density="compact">
              {advice.map((a) => (
                <ListItem
                  key={a.id}
                  label={a.title}
                  description={a.action}
                  startContent={<StatusDot variant={a.severity === 'error' ? 'error' : 'warning'} label={a.severity === 'error' ? 'needs action' : 'check'} />}
                  endContent={
                    <Link href={a.href} isExternalLink>
                      What to do
                    </Link>
                  }
                />
              ))}
            </List>
          ) : (
            <Text as="p">No trust-anchor state needs you on this node.</Text>
          )}
          <MetadataList columns="multi">
            <MetadataListItem label="Anchor mode">{`${s.anchorMode}${s.managedTopology ? ` (${s.managedTopology})` : ''}`}</MetadataListItem>
            <MetadataListItem label="Owner">{`${s.owner.phase}${s.owner.generation ? `, generation ${s.owner.generation}` : ''}`}</MetadataListItem>
            <MetadataListItem label="Authority">{`${s.authority.mode}, term ${s.authority.termEpoch} (installed ${s.authority.installedTermEpoch})`}</MetadataListItem>
            <MetadataListItem label="Fenced behind its term">{s.authority.fencedBehindTerm ? 'yes' : 'no'}</MetadataListItem>
            <MetadataListItem label="Updater">{worker(s.updater)}</MetadataListItem>
            <MetadataListItem label="Root check">{worker(s.supervisor)}</MetadataListItem>
            {s.pull ? (
              <MetadataListItem label="Last pull">{`${s.pull.result} at ${formatDateTime(s.pull.at)}${s.pull.refusal ? `; refuses the primary's state (${s.pull.refusal}) since ${formatDateTime(s.pull.refusedSince)}` : ''}`}</MetadataListItem>
            ) : null}
            {s.owner.nextRefresh ? <MetadataListItem label="Next refresh">{formatDateTime(s.owner.nextRefresh)}</MetadataListItem> : null}
            {s.local ? <MetadataListItem label="Local denials">{`${s.local.denied?.length ?? 0}${s.local.retired ? ', output retired' : ''}${s.local.terminal ? ', terminal' : ''}`}</MetadataListItem> : null}
          </MetadataList>
          {s.owner.keys?.length ? <Table<KeyRow> aria-label="Root keys" density="compact" idKey="id" data={s.owner.keys} columns={keyColumns} /> : null}
          {s.warnings?.length ? (
            <List aria-label="Warnings of this node" density="compact">
              {s.warnings.map((w) => (
                <ListItem key={w} label={w} />
              ))}
            </List>
          ) : null}
          <Link href={CLUSTER_DOCS.anchorsReturn} isExternalLink>
            Managed root anchors on returning, fenced and conflicted members
          </Link>
        </VStack>
      ) : null}
    </Panel>
  );
}
