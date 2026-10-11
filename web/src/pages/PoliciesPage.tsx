// Policies tie client groups to block and allow lists, blocked services,
// safe search, a block response and a schedule (§7.2). The list is a
// table in priority order with each policy's state right now (active,
// scheduled off, disabled, shadowed: derived from the objects, the same
// rules the server applies), its groups, lists and service logos; a row
// opens the policy's page, "New policy" the wizard.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMemo } from 'react';
import { useCan } from '../api/context';
import type { Policy } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ManagedBadge } from '../components/ManagedBadge';
import { ServiceIcon } from '../components/ServiceIcon';
import { useCatalogueServices } from '../components/ServiceGrid';
import { MANAGED_BY, objectComment } from '../lib/managedBy';
import { derivePolicyState, objectHref, policyBadge, refOfObject, scheduleSummary, sortByPriority } from '../lib/policies';
import { Link, useNavigate, useSearchParams } from '../lib/router';
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

export default function PoliciesPage() {
  const can = useCan();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const { ctx, ready, error } = useDeriveContext();
  const services = useCatalogueServices();
  const byId = useMemo(() => new Map((services.data?.items ?? []).map((s) => [s.id, s])), [services.data]);

  const rows = useMemo(() => {
    const s = search.trim().toLowerCase();
    const all = sortByPriority(ctx.policies);
    const shown = s
      ? all.filter((p) => p.metadata.name.toLowerCase().includes(s) || objectComment(p).toLowerCase().includes(s) || (p.spec.clientGroupRefs ?? []).some((g) => g.includes(s)))
      : all;
    return shown.map((p) => ({ p, state: derivePolicyState(p, ctx) }));
  }, [ctx, search]);

  const canWrite = can('policies', 'write');
  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Policies</h1>
          <p className="k-muted k-small">
            Which lists and services apply to which client groups, highest priority first. When several active policies match a client, the highest priority decides the settings and their lists are merged.
          </p>
        </div>
        <div className="k-actions">
          <Button label="New policy" variant="primary" isDisabled={!canWrite} tooltip={canWrite ? undefined : 'Needs policies.write'} onClick={() => navigate('/policies/new')} />
        </div>
      </div>
      <TextInput className="k-search" label="Search policies" isLabelHidden placeholder="Search by name, comment or group" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
      {error ? <ErrorBanner error={error} /> : null}
      {!ready && !error ? <Loading /> : null}
      {ready ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Policies">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">State</th>
                <th scope="col" className="num">
                  Priority
                </th>
                <th scope="col">Groups</th>
                <th scope="col">Lists</th>
                <th scope="col">Services</th>
                <th scope="col">Schedule</th>
                <th scope="col">Managed by</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <td colSpan={8} className="k-muted">
                    {search ? `No policy matches "${search}".` : 'No policies yet.'}
                  </td>
                </tr>
              ) : null}
              {rows.map(({ p, state }) => (
                <PolicyRow key={refOfObject(p)} p={p} badge={policyBadge(state)} services={byId} />
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}

function PolicyRow({ p, badge, services }: { p: Policy; badge: ReturnType<typeof policyBadge>; services: Map<string, { name: string; icon?: string }> }) {
  const ns = p.metadata.namespace ?? 'default';
  const comment = objectComment(p);
  const blocked = p.spec.blockedServices ?? [];
  return (
    <tr data-testid={`row-${p.metadata.name}`} data-state={badge.key}>
      <td>
        <Link href={objectHref('policies', refOfObject(p))} className="k-row-link">
          {p.metadata.name}
        </Link>
        {ns !== 'default' ? <span className="k-muted k-small"> ({ns})</span> : null}
        {comment ? <div className="k-muted k-small">{comment}</div> : null}
      </td>
      <td>
        <span title={badge.help}>
          <Badge variant={badge.variant} label={badge.label} />
        </span>
      </td>
      <td className="num">{p.spec.priority ?? 0}</td>
      <td>
        <Refs kind="groups" ns={ns} names={p.spec.clientGroupRefs} />
      </td>
      <td>
        {p.spec.allowOnly ? <Badge variant="warning" label="allow only" /> : null}
        <Refs kind="lists" ns={ns} names={[...(p.spec.blockListRefs ?? []), ...(p.spec.allowListRefs ?? [])]} />
      </td>
      <td>
        {blocked.length || p.spec.safeSearch ? (
          <ul className="k-logo-stack" aria-label={`Services blocked by ${p.metadata.name}`}>
            {blocked.slice(0, SHOWN_SERVICES).map((id) => (
              <li key={id} title={services.get(id)?.name ?? id}>
                <ServiceIcon id={id} name={services.get(id)?.name ?? id} icon={services.get(id)?.icon} size={22} />
                <span className="k-visually-hidden">{services.get(id)?.name ?? id}</span>
              </li>
            ))}
            {blocked.length > SHOWN_SERVICES ? <li className="k-muted k-small">+{blocked.length - SHOWN_SERVICES}</li> : null}
            {p.spec.safeSearch ? (
              <li>
                <Badge variant="teal" label="safe search" />
              </li>
            ) : null}
          </ul>
        ) : (
          <span className="k-muted">none</span>
        )}
      </td>
      <td className="k-small">{p.spec.schedule ? scheduleSummary(p.spec.schedule) : <span className="k-muted">always</span>}</td>
      <td>
        <ManagedBadge owner={p.metadata.annotations?.[MANAGED_BY]} />
      </td>
    </tr>
  );
}
