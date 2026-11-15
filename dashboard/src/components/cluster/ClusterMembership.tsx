// The Nodes page's membership panel on the SQLite driver
// (docs/deploy/sqlite-cluster.md): this node's enrollment (GET
// /cluster/enrollment) and the next step its mode allows: activate a dormant
// standalone node, invite a member from the primary, review the promotion of
// a follower. The commands run in the wizards of ClusterWizards.tsx.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCannotReason } from '../../api/context';
import type { ClusterActivationReceipt, ClusterPromotionReceipt } from '../../api/types';
import { CLUSTER_DOCS } from '../../lib/cluster';
import { ErrorBanner, Loading } from '../Feedback';
import { Panel } from '../layout/Page';
import { ActivateDialog, clusterKeys, InviteDialog, PromoteDialog, refreshCluster } from '../wizards/ClusterWizards';

type Open = 'activate' | 'invite' | 'promote' | null;

function modeText(mode: string, dormant: boolean): string {
  switch (mode) {
    case 'standalone':
      return dormant ? 'This node is not in a cluster. Activate it to make it the first member and the primary of an enrolled cluster.' : 'This node is standalone.';
    case 'primary':
      return 'This node is the primary: it accepts every change and its followers copy its data. Invite each new member from here.';
    case 'follower':
      return 'This node is a follower: it copies the primary\'s data and answers DNS from its own copy, also while the primary is away. Promote it only when the primary is lost.';
    case 'fenced':
      return 'This node observed a newer primary and follows it after its next successful pull.';
    case 'conflict':
      return 'This node recorded conflicting promotions; an operator decides which member to keep.';
    default:
      return `Mode ${mode}.`;
  }
}

export function ClusterMembership({ node }: { node: string }) {
  const api = useApi();
  const qc = useQueryClient();
  const cannot = useCannotReason();
  const enrollment = useQuery({ queryKey: clusterKeys.enrollment, queryFn: () => api.cluster.enrollment(), retry: false });
  const version = useQuery({ queryKey: qk.version, queryFn: () => api.cluster.version() });
  const [open, setOpen] = useState<Open>(null);
  const [activated, setActivated] = useState<ClusterActivationReceipt>();
  const [promoted, setPromoted] = useState<ClusterPromotionReceipt>();
  const e = enrollment.data;
  const joinTip = cannot('cluster', 'join');
  const promoteTip = cannot('cluster', 'promote');
  return (
    <Panel
      title="Cluster membership"
      headingId="membership-title"
      description="Enrolled SQLite cluster: one primary accepts every change and the followers copy it. Joining runs on the new node with an offline command; this page reviews, activates, invites and promotes."
      actions={
        e ? (
          <>
            {e.mode === 'standalone' && e.dormant ? <Button label="Activate cluster" variant="primary" isDisabled={!!joinTip} tooltip={joinTip} onClick={() => setOpen('activate')} /> : null}
            {e.mode === 'primary' ? <Button label="Invite a member" variant="primary" isDisabled={!!joinTip} tooltip={joinTip} onClick={() => setOpen('invite')} /> : null}
            {e.mode === 'follower' ? <Button label="Review promotion" variant="secondary" isDisabled={!!promoteTip} tooltip={promoteTip} onClick={() => setOpen('promote')} /> : null}
          </>
        ) : null
      }
    >
      {activated ? (
        <Banner
          status="success"
          title="Cluster activated: restart kudurru on this node"
          description={`Member ${activated.memberID} of cluster ${activated.clusterID} is committed as the primary. Until kudurru restarts on this node its member listener does not run, so followers cannot join yet. After the restart, invite the followers here.`}
          isDismissable
          onDismiss={() => setActivated(undefined)}
        />
      ) : null}
      {promoted ? (
        <Banner
          status="success"
          title={`Promoted: this node is the primary of term ${String(promoted.term.epoch)}`}
          description="The promotion is committed. The other members follow at their next pull; a returning old primary is fenced and follows too. This does not say that every member has converged yet."
          isDismissable
          onDismiss={() => setPromoted(undefined)}
        />
      ) : null}
      {enrollment.error ? <ErrorBanner error={enrollment.error} /> : null}
      {enrollment.isLoading ? <Loading label="Reading the enrollment" /> : null}
      {e ? (
        <VStack gap={2}>
          <Text as="p">{modeText(e.mode, e.dormant)}</Text>
          <MetadataList columns="multi">
            <MetadataListItem label="Mode">{`${e.mode}${e.dormant ? ' (dormant)' : ''}`}</MetadataListItem>
            <MetadataListItem label="Cluster ID">
              <Text type="code" size="sm">
                {e.clusterID}
              </Text>
            </MetadataListItem>
            <MetadataListItem label="Member ID">
              <Text type="code" size="sm">
                {e.memberID}
              </Text>
            </MetadataListItem>
          </MetadataList>
          <HStack gap={3} wrap="wrap">
            <Link href={CLUSTER_DOCS.cluster} isExternalLink>
              Enrolled SQLite cluster guide
            </Link>
            <Link href={CLUSTER_DOCS.join} isExternalLink>
              Join a new member
            </Link>
          </HStack>
        </VStack>
      ) : null}
      <ActivateDialog
        isOpen={open === 'activate'}
        onClose={() => setOpen(null)}
        node={node}
        onActivated={(r) => {
          setOpen(null);
          setActivated(r);
          refreshCluster(qc);
        }}
      />
      <InviteDialog isOpen={open === 'invite'} onClose={() => setOpen(null)} version={version.data?.version} />
      <PromoteDialog
        isOpen={open === 'promote'}
        onClose={() => setOpen(null)}
        onPromoted={(r) => {
          setOpen(null);
          setPromoted(r);
          refreshCluster(qc);
        }}
      />
    </Panel>
  );
}
