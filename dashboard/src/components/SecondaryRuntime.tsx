// Secondary availability is local to the API node, independent of shared status.
import { Banner } from '@astryxdesign/core/Banner';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { SecondaryRuntime } from '../api/types';
import { formatDateTime, formatExact } from '../lib/format';
import { Panel } from './layout/Page';

const STATE_LABELS = { pending: 'Pending transfer', serving: 'Serving', expired: 'Expired', unavailable: 'Unavailable' } as const;

/** Reused in the zone list and detail header; absent state never means Ready. */
export function SecondaryAvailability({ runtime }: { runtime: SecondaryRuntime | undefined }) {
  return (
    <VStack gap={0.5}>
      <Text size="sm">{runtime ? STATE_LABELS[runtime.state] : 'Unknown'}</Text>
      <Text type="supporting">{runtime ? `on node ${runtime.node}` : 'on this node'}</Text>
    </VStack>
  );
}

export function SecondaryRuntimeSection({ runtime }: { runtime: SecondaryRuntime | undefined }) {
  return (
    <Panel title="Secondary on this node" description="Availability and timestamps describe only the responding node's copy of this zone.">
      <VStack gap={4}>
        <SecondaryAvailability runtime={runtime} />
        {runtime ? (
          <>
            <MetadataList columns="multi">
              <MetadataListItem label="Node">{runtime.node}</MetadataListItem>
              <MetadataListItem label={runtime.state === 'serving' ? 'Served serial' : 'Last snapshot serial'}>{runtime.serial ?? '–'}</MetadataListItem>
              <MetadataListItem label={runtime.state === 'serving' ? 'Served records' : 'Last snapshot records'}>{formatExact(runtime.recordCount)}</MetadataListItem>
              <MetadataListItem label="Primary">{runtime.primary ?? '–'}</MetadataListItem>
              <MetadataListItem label="Last confirmed">{formatDateTime(runtime.lastConfirmedAt)}</MetadataListItem>
              <MetadataListItem label="Expires at">{formatDateTime(runtime.expiresAt)}</MetadataListItem>
              <MetadataListItem label="Next attempt">{formatDateTime(runtime.nextAttemptAt)}</MetadataListItem>
            </MetadataList>
            {runtime.reason || runtime.message ? (
              <Banner
                status={runtime.state === 'serving' || runtime.state === 'pending' ? 'info' : 'error'}
                title={runtime.reason || 'Secondary transfer'}
                description={runtime.message}
              />
            ) : null}
          </>
        ) : <Text type="supporting">Secondary runtime information is unavailable. This node's served serial and expiry are unknown.</Text>}
      </VStack>
    </Panel>
  );
}
