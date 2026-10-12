// What a page shows instead of its content when its feature needs a license
// key (GET /license `features`): a short explanation and the way forward.
// The page makes no request for the locked data, so the server logs no 402.
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Link } from '@astryxdesign/core/Link';
import { Text } from '@astryxdesign/core/Text';
import { Lock } from 'lucide-react';
import { featureLabel, lockedSentence } from '../lib/license';

export function FeatureLocked({ feature, what }: { feature: string; what?: string }) {
  const name = what ?? featureLabel(feature);
  return (
    <EmptyState
      data-testid={`locked-${feature}`}
      icon={<Lock size={32} aria-hidden="true" />}
      title={`${name} needs a license key`}
      description={lockedSentence(name)}
      actions={
        <Text>
          <Link href="/license">View license</Link>
        </Text>
      }
    />
  );
}
