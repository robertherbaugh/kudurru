// The follower notice above every page (D368): this node is an enrolled
// SQLite follower, so the changes it forwards reach its primary and appear
// here after the next pull, and the rest are made on the primary. It stays
// as long as the role lasts, like the static-mode read-only state.
import { Banner } from '@astryxdesign/core/Banner';
import { Link } from '@astryxdesign/core/Link';
import { useClusterRole } from '../api/context';

export function FollowerBanner() {
  const role = useClusterRole();
  if (!role.follower) return null;
  const primary = role.primary ?? 'the primary';
  return (
    <Banner
      data-testid="follower-banner"
      status="info"
      container="section"
      title={role.primary ? `This node is a follower of ${role.primary}` : 'This node is a follower'}
      description={`Changes to zones, lists, groups, policies, settings, blocking and your own account are sent to ${primary} and show here after the next sync, within a few seconds. Other administration (users and role bindings, allow and deny entries, unblock decisions, TSIG keys, integrations, imports, list refresh and NOTIFY) runs only on ${primary}; those controls are disabled here.`}
      endContent={<Link href="/nodes">Cluster details</Link>}
    />
  );
}
