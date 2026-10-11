// Policies tie client groups to block and allow lists, blocked services,
// safe search, a block response and a schedule (§7.2). The list (the
// shared ObjectsPage) is in priority order with each policy's state right
// now (active, scheduled off, disabled, shadowed: derived from the
// objects, the same rules the server applies), its groups, lists and
// service logos; a name opens the policy's page, "New policy" the wizard.
import { Badge } from '@astryxdesign/core/Badge';
import { useMemo } from 'react';
import type { Policy } from '../api/types';
import { ObjectsPage } from '../components/ObjectsPage';
import { ServiceIcon } from '../components/ServiceIcon';
import { useCatalogueServices } from '../components/ServiceGrid';
import { SCHEMAS } from '../lib/objectSchemas';
import { derivePolicyState, objectHref, policyBadge, refOfObject, scheduleSummary } from '../lib/policies';
import { Link } from '../lib/router';
import { useDeriveContext } from '../lib/usePolicyData';

const SHOWN_SERVICES = 5;

function Refs({ kind, ns, names }: { kind: 'groups' | 'lists'; ns: string; names: string[] | undefined }) {
  if (!names?.length) return <span className="k-muted">none</span>;
  return (
    <span className="k-small">
      {names.map((n, i) => (
        <span key={n}>
          {i ? ', ' : ''}
          <Link href={objectHref(kind, `${ns}/${n}`)}>{n}</Link>
        </span>
      ))}
    </span>
  );
}

/** Highest priority first, ties by namespace/name (internal/policy). */
function byPriority(a: Policy, b: Policy): number {
  return (b.spec.priority ?? 0) - (a.spec.priority ?? 0) || refOfObject(a).localeCompare(refOfObject(b));
}

export default function PoliciesPage() {
  const { ctx } = useDeriveContext();
  const services = useCatalogueServices();
  const byId = useMemo(() => new Map((services.data?.items ?? []).map((s) => [s.id, s])), [services.data]);
  const badges = useMemo(() => new Map(ctx.policies.map((p) => [refOfObject(p), policyBadge(derivePolicyState(p, ctx))])), [ctx]);

  return (
    <ObjectsPage<Policy>
      title="Policies"
      description="Which lists and services apply to which client groups, highest priority first. When several active policies match a client, the highest priority decides the settings and their lists are merged."
      schema={SCHEMAS.policies}
      detailPath={(p) => objectHref('policies', refOfObject(p))}
      createPath="/policies/new"
      sort={byPriority}
      columns={[
        {
          header: 'State',
          cell: (p) => {
            const b = badges.get(refOfObject(p));
            return b ? (
              <span title={b.help} data-state={b.key}>
                <Badge variant={b.variant} label={b.label} />
              </span>
            ) : null;
          },
        },
        { header: 'Priority', className: 'num', cell: (p) => p.spec.priority ?? 0 },
        { header: 'Groups', cell: (p) => <Refs kind="groups" ns={p.metadata.namespace ?? 'default'} names={p.spec.clientGroupRefs} /> },
        {
          header: 'Lists',
          cell: (p) => (
            <>
              {p.spec.allowOnly ? <Badge variant="warning" label="allow only" /> : null}
              <Refs kind="lists" ns={p.metadata.namespace ?? 'default'} names={[...(p.spec.blockListRefs ?? []), ...(p.spec.allowListRefs ?? [])]} />
            </>
          ),
        },
        {
          header: 'Services',
          cell: (p) => {
            const blocked = p.spec.blockedServices ?? [];
            if (!blocked.length && !p.spec.safeSearch) return <span className="k-muted">none</span>;
            return (
              <ul className="k-logo-stack" aria-label={`Services blocked by ${p.metadata.name}`}>
                {blocked.slice(0, SHOWN_SERVICES).map((id) => (
                  <li key={id} title={byId.get(id)?.name ?? id}>
                    <ServiceIcon id={id} name={byId.get(id)?.name ?? id} icon={byId.get(id)?.icon} size={22} />
                    <span className="k-visually-hidden">{byId.get(id)?.name ?? id}</span>
                  </li>
                ))}
                {blocked.length > SHOWN_SERVICES ? <li className="k-muted k-small">+{blocked.length - SHOWN_SERVICES}</li> : null}
                {p.spec.safeSearch ? (
                  <li>
                    <Badge variant="teal" label="safe search" />
                  </li>
                ) : null}
              </ul>
            );
          },
        },
        { header: 'Schedule', cell: (p) => (p.spec.schedule ? <span className="k-small">{scheduleSummary(p.spec.schedule)}</span> : <span className="k-muted">always</span>) },
      ]}
    />
  );
}
