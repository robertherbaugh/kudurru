// Pending unblock requests: the count next to "Requests" in the
// navigation and the dashboard notice. Both show nothing when no request
// waits, without unblockrequests.read, or when the API answers 403.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { usePendingUnblockCount } from '../api/hooks';
import { Link } from '../lib/router';
import { requestsLabel } from '../lib/unblock';

export function UnblockBadge() {
  const q = usePendingUnblockCount();
  const n = q.data ?? 0;
  if (q.error || n <= 0) return null;
  return (
    <span data-testid="requests-badge">
      <Badge variant="warning" label={String(n)} aria-label={`${requestsLabel(n)} waiting`} />
    </span>
  );
}

export function UnblockNotice() {
  const q = usePendingUnblockCount();
  const n = q.data ?? 0;
  if (q.error || n <= 0) return null;
  return (
    <div data-testid="requests-notice">
      <Banner
        status="info"
        title={`${requestsLabel(n)} waiting`}
        description="Users asked on the block page for blocked names to be allowed."
        endContent={<Link href="/requests">Review requests</Link>}
      />
    </div>
  );
}
