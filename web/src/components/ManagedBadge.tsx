// "Managed by file / Kubernetes / integration" badge with the owner in the
// tooltip and accessible name (§10 principle 6).
import { Badge } from '@astryxdesign/core/Badge';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { ownership } from '../lib/managedBy';

export function ManagedBadge({ owner }: { owner: string | undefined }) {
  const o = ownership(owner);
  const variant = o.kind === 'file' ? 'orange' : o.kind === 'kubernetes' ? 'blue' : o.kind === 'integration' ? 'purple' : o.kind === 'bootstrap' ? 'teal' : 'neutral';
  const text = o.kind === 'file' || o.kind === 'kubernetes' || o.kind === 'integration' ? `managed by ${o.label}` : o.label;
  return (
    <Tooltip content={o.owner ? `kudurru.io/managed-by: ${o.owner}` : 'No manager annotation'}>
      <Badge variant={variant} label={text} aria-label={o.owner ? `Managed by ${o.owner}` : 'Unmanaged'} />
    </Tooltip>
  );
}
