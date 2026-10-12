// Policies tie client groups to block and allow lists, blocked services,
// safe search, a block response and a schedule (§7.2). The list (the
// shared ObjectsPage) is in priority order with each policy's state right
// now (active, scheduled off, disabled, shadowed: derived from the
// objects, the same rules the server applies), its groups, lists and
// service logos; a name opens the policy's page, "New policy" the wizard.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useMemo } from 'react';
import { useLicense } from '../api/hooks';
import type { Policy } from '../api/types';
import { ObjectsPage } from '../components/ObjectsPage';
import { PolicyWizardDialog } from '../components/wizards/PolicyWizard';
import { LogoStack, useCatalogueServices } from '../components/ServiceGrid';
import { SCHEMAS } from '../lib/objectSchemas';
import { derivePolicyState, objectHref, policyBadge, refOfObject, scheduleSummary } from '../lib/policies';
import { Link } from '@astryxdesign/core/Link';
import { useDeriveContext } from '../lib/usePolicyData';

const SHOWN_SERVICES = 5;

function Refs({ kind, ns, names }: { kind: 'groups' | 'lists'; ns: string; names: string[] | undefined }) {
  if (!names?.length) return <Text color="secondary">none</Text>;
  return (
    <Text size="sm">
      {names.map((n, i) => (
        <Text key={n} type="inherit">
          {i ? ', ' : ''}
          <Link href={objectHref(kind, `${ns}/${n}`)} type="inherit" color="inherit" hasUnderline>
            {n}
          </Link>
        </Text>
      ))}
    </Text>
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

  const license = useLicense();
  const limit = license.data?.policyLimit ?? 0;
  const atLimit = limit > 0 && ctx.policies.length >= limit;

  return (
    <ObjectsPage<Policy>
      title="Policies"
      createBlocked={atLimit ? `Without a license key at most ${limit} policies can exist` : undefined}
      description="Which lists and services apply to which client groups, highest priority first. When several active policies match a client, the highest priority decides the settings and their lists are merged."
      schema={SCHEMAS.policies}
      detailPath={(p) => objectHref('policies', refOfObject(p))}
      wizard={PolicyWizardDialog}
      sort={byPriority}
      columns={[
        {
          header: 'State',
          cell: (p) => {
            const b = badges.get(refOfObject(p));
            return b ? (
              <Tooltip content={b.help}>
                <Badge variant={b.variant} label={b.label} data-state={b.key} />
              </Tooltip>
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
            if (!blocked.length && !p.spec.safeSearch) return <Text color="secondary">none</Text>;
            return (
              <LogoStack
                label={`Services blocked by ${p.metadata.name}`}
                items={blocked.slice(0, SHOWN_SERVICES).map((id) => {
                  const s = byId.get(id);
                  return { id, name: s?.name ?? id, icon: s?.icon, iconOnDark: s?.iconOnDark };
                })}
                more={blocked.length - SHOWN_SERVICES}
                extra={p.spec.safeSearch ? <Badge variant="teal" label="safe search" /> : undefined}
              />
            );
          },
        },
        { header: 'Schedule', cell: (p) => (p.spec.schedule ? <Text size="sm">{scheduleSummary(p.spec.schedule)}</Text> : <Text color="secondary">always</Text>) },
      ]}
    >
      {atLimit ? (
        <Banner
          data-testid="policy-limit"
          status="info"
          title={`The free limit of ${limit} policies is reached`}
          description="Existing policies keep working. A license key lifts the limit."
          endContent={<Link href="/license">View license</Link>}
        />
      ) : null}
    </ObjectsPage>
  );
}
