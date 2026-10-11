// "Managed by file / Kubernetes / integration" badge with the owner in the
// tooltip and accessible name (§10 principle 6).
import { Badge } from '@astryxdesign/core/Badge';
import { ownership } from '../lib/managedBy';

export function ManagedBadge({ owner }: { owner: string | undefined }) {
  const o = ownership(owner);
  const variant = o.kind === 'file' ? 'orange' : o.kind === 'kubernetes' ? 'blue' : o.kind === 'integration' ? 'purple' : o.kind === 'bootstrap' ? 'teal' : 'neutral';
  const text = o.kind === 'file' || o.kind === 'kubernetes' || o.kind === 'integration' ? `managed by ${o.label}` : o.label;
  return (
    <span title={o.owner ? `kudurru.io/managed-by: ${o.owner}` : 'No manager annotation'} aria-label={o.owner ? `Managed by ${o.owner}` : 'Unmanaged'}>
      <Badge variant={variant} label={text} />
    </span>
  );
}
