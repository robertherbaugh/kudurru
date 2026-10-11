// Client groups: who is who. The bootstrap "default" group matches every
// client, so new devices are auto-assigned to it until a more specific
// group claims them. The list (the shared ObjectsPage) shows each group's
// matchers, the members the controller counted and the policies that
// apply, in priority order; a name opens the group's page, "New group"
// the wizard.
import type { ClientGroup } from '../api/types';
import { describeMatcher } from '../components/fields/MatchersField';
import { ObjectsPage } from '../components/ObjectsPage';
import { formatExact } from '../lib/format';
import { SCHEMAS } from '../lib/objectSchemas';
import { objectHref, policiesOfGroup, refOfObject } from '../lib/policies';
import { Link } from '../lib/router';
import { usePolicies } from '../lib/usePolicyData';

export default function GroupsPage() {
  const pols = usePolicies();
  return (
    <ObjectsPage<ClientGroup>
      title="Groups"
      description='Client groups match devices by IP address, CIDR range, client ID or MAC address. Clients no other group claims fall into "default".'
      schema={SCHEMAS.clientgroups}
      detailPath={(g) => objectHref('groups', refOfObject(g))}
      createPath="/groups/new"
      columns={[
        {
          header: 'Members',
          cell: (g) => {
            const list = g.spec.clients ?? [];
            return (
              <span className="k-small k-break">
                {list.slice(0, 4).map(describeMatcher).join(', ') || <span className="k-muted">none</span>}
                {list.length > 4 ? ` and ${list.length - 4} more` : ''}
                {g.spec.sources?.integrationRef ? ` + integration ${g.spec.sources.integrationRef.name}` : ''}
              </span>
            );
          },
        },
        { header: 'Seen', className: 'num', cell: (g) => formatExact(g.status?.members) },
        {
          header: 'Policies',
          cell: (g) => {
            const applied = policiesOfGroup(g, pols.data?.items ?? []);
            if (!applied.length) return <span className="k-muted k-small">default policy</span>;
            return (
              <span className="k-small">
                {applied.map((p, i) => (
                  <span key={p.metadata.name}>
                    {i ? ', ' : ''}
                    <Link href={objectHref('policies', refOfObject(p))}>{p.metadata.name}</Link>
                  </span>
                ))}
              </span>
            );
          },
        },
      ]}
    />
  );
}
