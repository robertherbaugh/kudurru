// Policies tie client groups to block and allow lists, with priority,
// schedule, upstream group and rewrites (§7.2).
import { Badge } from '@astryxdesign/core/Badge';
import { ObjectsPage } from '../components/ObjectsPage';
import type { Policy } from '../api/types';
import { SCHEMAS } from '../lib/objectSchemas';

function refs(names: string[] | undefined) {
  if (!names?.length) return <span className="k-muted">none</span>;
  return <span className="k-small">{names.join(', ')}</span>;
}

export default function PoliciesPage() {
  return (
    <ObjectsPage<Policy>
      title="Policies"
      description="Which lists apply to which client groups. When several policies match a client, the highest priority wins; ties are reported on the Diagnosis page."
      schema={SCHEMAS.policies}
      columns={[
        { header: 'Priority', className: 'num', cell: (p) => p.spec.priority ?? 0 },
        { header: 'Groups', cell: (p) => refs(p.spec.clientGroupRefs) },
        { header: 'Block lists', cell: (p) => (p.spec.allowOnly ? <Badge variant="warning" label="allow only" /> : refs(p.spec.blockListRefs)) },
        { header: 'Allow lists', cell: (p) => refs(p.spec.allowListRefs) },
        {
          header: 'Schedule',
          cell: (p) =>
            p.spec.schedule ? (
              <span className="k-small">
                {(p.spec.schedule.weekdays ?? []).join(' ') || 'daily'} {p.spec.schedule.start}–{p.spec.schedule.end}
                {p.spec.schedule.tz ? ` ${p.spec.schedule.tz}` : ''}
              </span>
            ) : (
              <span className="k-muted">always</span>
            ),
        },
      ]}
    />
  );
}
