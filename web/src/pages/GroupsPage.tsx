// Client groups: who is who. The bootstrap "default" group matches every
// client, so new devices are auto-assigned to it until a more specific
// group claims them.
import { ObjectsPage } from '../components/ObjectsPage';
import type { ClientGroup } from '../api/types';
import { formatExact } from '../lib/format';
import { SCHEMAS } from '../lib/objectSchemas';

export function describeMatcher(c: NonNullable<ClientGroup['spec']['clients']>[number]): string {
  const what = c.ip ?? c.cidr ?? c.mac ?? (c.clientID ? `id ${c.clientID}` : undefined) ?? c.hostname ?? c.nameGlob ?? '?';
  return c.name ? `${c.name} (${what})` : what;
}

export default function GroupsPage() {
  return (
    <ObjectsPage<ClientGroup>
      title="Groups"
      description='Client groups match devices by IP, CIDR, MAC, client ID or name. Clients no other group claims fall into "default".'
      schema={SCHEMAS.clientgroups}
      columns={[
        {
          header: 'Clients',
          cell: (g) => {
            const list = g.spec.clients ?? [];
            const shown = list.slice(0, 4).map(describeMatcher).join(', ');
            return (
              <span className="k-small k-break">
                {shown}
                {list.length > 4 ? ` and ${list.length - 4} more` : ''}
                {g.spec.sources?.integrationRef ? ` + integration ${g.spec.sources.integrationRef.name}` : ''}
              </span>
            );
          },
        },
        { header: 'Members seen', className: 'num', cell: (g) => formatExact(g.status?.members) },
      ]}
    />
  );
}
