// The diagnosis badge (§10 principle 8): the number of GET /info/messages
// entries, coloured by the worst severity, linking to the Diagnosis page.
import { Badge } from '@astryxdesign/core/Badge';
import { useMessages } from '../api/hooks';
import { Link } from '@astryxdesign/core/Link';

export function MessagesBadge({ compact }: { compact?: boolean }) {
  const q = useMessages();
  const items = q.data?.items ?? [];
  if (items.length === 0) return null;
  const worst = items.some((m) => m.severity === 'error') ? 'error' : items.some((m) => m.severity === 'warning') ? 'warning' : 'info';
  const label = `${items.length} diagnosis message${items.length === 1 ? '' : 's'}`;
  if (compact) return <Badge variant={worst} label={String(items.length)} aria-label={label} />;
  return (
    <Link href="/messages" label={label}>
      <Badge variant={worst} label={`${items.length} ${worst === 'info' ? 'notices' : worst === 'error' ? 'errors' : 'warnings'}`} />
    </Link>
  );
}
