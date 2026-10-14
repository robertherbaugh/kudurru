// Stub routes are discovered independently on every serving node.
import { Banner } from '@astryxdesign/core/Banner';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { StubRuntime } from '../api/types';
import { formatDateTime } from '../lib/format';
import { Panel } from './layout/Page';

const STATE_LABELS = { pending: 'Pending discovery', routing: 'Routing', expired: 'Expired', unavailable: 'Unavailable' } as const;

export function StubAvailability({ runtime }: { runtime: StubRuntime | undefined }) {
  return (
    <VStack gap={0.5}>
      <Text size="sm">{runtime ? STATE_LABELS[runtime.state] : 'Unknown'}</Text>
      <Text type="supporting">{runtime ? `on node ${runtime.node}` : 'on this node'}</Text>
    </VStack>
  );
}

export function StubRuntimeSection({ runtime }: { runtime: StubRuntime | undefined }) {
  return (
    <Panel title="Stub on this node" description="The responding node discovers and refreshes this route independently. An expired route answers SERVFAIL.">
      <VStack gap={4}>
        <StubAvailability runtime={runtime} />
        {runtime ? (
          <>
            <MetadataList columns="multi">
              <MetadataListItem label="Node">{runtime.node}</MetadataListItem>
              <MetadataListItem label="Last observed SOA serial">{runtime.serial ?? '–'}</MetadataListItem>
              <MetadataListItem label="Authorities">{runtime.authorities.length ? runtime.authorities.map((server) => `${server.name} (${server.protocol}://${server.address})`).join(', ') : '–'}</MetadataListItem>
              <MetadataListItem label="Last confirmed">{formatDateTime(runtime.lastConfirmedAt)}</MetadataListItem>
              <MetadataListItem label="Expires at">{formatDateTime(runtime.expiresAt)}</MetadataListItem>
              <MetadataListItem label="Next attempt">{formatDateTime(runtime.nextAttemptAt)}</MetadataListItem>
            </MetadataList>
            {runtime.reason || runtime.message ? (
              <Banner status={runtime.state === 'routing' || runtime.state === 'pending' ? 'info' : 'error'} title={runtime.reason || 'Stub discovery'} description={runtime.message} />
            ) : null}
          </>
        ) : <Text type="supporting">Stub runtime information is unavailable. This node's route and expiry are unknown.</Text>}
      </VStack>
    </Panel>
  );
}
