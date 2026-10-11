// One client group (/groups/:ns/:name): its matchers as a typed editor
// (IP address, CIDR range, client ID, MAC address, each validated), the
// clients of the last 24 hours that resolved to it, and the policies that
// apply to its members in the order the server ranks them. Enable/disable,
// delete and "View as YAML" sit in the header.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, ClientMatcher } from '../api/types';
import { EditableSection } from '../components/EditableSection';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { cleanMatchers, kindLabel, matcherKind, matchersError, matcherValue, MatchersField } from '../components/fields/MatchersField';
import { FormDialog } from '../components/FormDialog';
import { ManagedBadge } from '../components/ManagedBadge';
import { YamlDialog } from '../components/YamlDialog';
import { formatExact, formatRelative } from '../lib/format';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { clientHref, DEFAULT_GROUP, derivePolicyState, identifyClient, objectHref, policiesOfGroup, policyBadge, refOfObject, scheduleSummary } from '../lib/policies';
import { Link, useNavigate } from '../lib/router';
import { useDeriveContext } from '../lib/usePolicyData';
import type { PageProps } from '../routes';

export default function GroupDetailPage({ params }: PageProps) {
  const ns = params.ns ?? 'default';
  const name = params.name ?? '';
  const ref = `${ns}/${name}`;
  const api = useApi();
  const can = useCan();
  const scope = useScopeParams();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [yaml, setYaml] = useState(false);

  const q = useQuery({ queryKey: [...qk.objects('clientgroups'), ns, name], queryFn: () => api.objects.get<ClientGroup>('clientgroups', name, ns) });
  const clients = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list({ window: '24h' }), refetchInterval: 60_000, enabled: can('clients', 'read') });
  const { ctx, ready } = useDeriveContext();
  const group = q.data;

  const put = async (next: ClientGroup) => {
    await api.objects.put('clientgroups', next);
    await qc.invalidateQueries({ queryKey: qk.objects('clientgroups') });
  };
  const toggle = useMutation({
    mutationFn: (enabled: boolean) => put(withMeta(group as ClientGroup, objectComment(group as ClientGroup), enabled)),
    onSuccess: (_r, enabled) => notify.success(`${name} ${enabled ? 'enabled' : 'disabled'}`),
    onError: (err) => notify.error(err, `Could not change ${name}`),
  });
  const del = useMutation({
    mutationFn: () => api.objects.delete('clientgroups', name, ns),
    onSuccess: () => {
      notify.success(`Deleted group ${name}`);
      // Only the list: refetching this page's object would answer 404.
      navigate('/groups');
      void qc.invalidateQueries({ queryKey: qk.objects('clientgroups'), exact: true });
    },
    onError: (err) => notify.error(err, 'Delete failed'),
  });

  // Clients the server resolved to this group, plus those the matchers
  // would put into it (a client can be in several groups of one tier).
  const members = useMemo(() => {
    const all = clients.data?.items ?? [];
    return all.filter((c) => c.group === ref || (ready && identifyClient(c.key, ctx.groups).groups.includes(ref)));
  }, [clients.data, ref, ready, ctx.groups]);

  if (q.error) {
    return (
      <div className="k-page">
        <Link href="/groups" className="k-small">
          ← Groups
        </Link>
        <h1>Group {name}</h1>
        <ErrorBanner error={q.error} />
      </div>
    );
  }
  if (!group) return <Loading />;

  const own = objectOwnership(group);
  const canWrite = can('clientgroups', 'write');
  const roReason = !own.editable ? readOnlyReason(own) : !canWrite ? 'Needs clientgroups.write' : '';
  const enabled = objectEnabled(group);
  const comment = objectComment(group);
  const matchers = group.spec.clients ?? [];
  const applied = policiesOfGroup(group, ctx.policies);
  const ready_ = group.status?.conditions?.find((c) => c.type === 'Ready');
  const problems = (group.status?.conditions ?? []).filter((c) => c.type !== 'Ready' && c.status === 'True');

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/groups" className="k-small">
            ← Groups
          </Link>
          <h1>{name}</h1>
          <span className="k-row k-small">
            <Badge variant={enabled ? 'success' : 'neutral'} label={enabled ? 'Enabled' : 'Disabled'} />
            <span>
              {matchers.length} matcher{matchers.length === 1 ? '' : 's'}
            </span>
            <span>{formatExact(group.status?.members ?? 0)} members counted</span>
            {ready_ ? <Badge variant={ready_.status === 'True' ? 'success' : 'error'} label={ready_.status === 'True' ? 'Ready' : 'Not ready'} /> : null}
            <ManagedBadge owner={group.metadata.annotations?.[MANAGED_BY]} />
          </span>
          {comment ? <p className="k-muted k-small">{comment}</p> : null}
        </div>
        <div className="k-actions">
          <Button label={enabled ? 'Disable' : 'Enable'} variant="secondary" isDisabled={!!roReason} tooltip={roReason || undefined} isLoading={toggle.isPending} onClick={() => toggle.mutate(!enabled)} />
          <Button label="View as YAML" variant="ghost" onClick={() => setYaml(true)} />
          <Button label="Delete" variant="destructive" isDisabled={!!roReason || !can('clientgroups', 'delete') || name === DEFAULT_GROUP} tooltip={name === DEFAULT_GROUP ? 'The default group is recreated at start-up' : roReason || undefined} onClick={() => setConfirmDelete(true)} />
        </div>
      </div>
      {!own.editable ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      {problems.map((c) => (
        <Banner key={c.type} status="warning" title={c.reason ?? c.type} description={c.message} />
      ))}

      <EditableSection<ClientMatcher[]>
        title="Members"
        description="A client is in the group when any one matcher matches it."
        value={matchers}
        readOnlyReason={roReason}
        testId="section-matchers"
        view={<MatchersView matchers={matchers} />}
        edit={(d, set, showErrors) => <MatchersField value={d} onChange={set} showErrors={showErrors} />}
        validate={matchersError}
        onSave={async (d) => {
          await put({ ...group, spec: { ...group.spec, clients: cleanMatchers(d) } });
          notify.success(`Saved the members of ${name}`);
        }}
      />

      <section className="k-panel k-section" aria-labelledby="sec-seen">
        <div className="k-panel-title">
          <h2 id="sec-seen">Seen in the last 24 hours</h2>
          <Link href="/clients">All clients</Link>
        </div>
        {clients.error ? <ErrorBanner error={clients.error} /> : null}
        {clients.isLoading ? <Loading /> : null}
        {clients.data ? (
          members.length ? (
            <div className="k-table-wrap">
              <table className="k-table" aria-label={`Clients of ${name}`}>
                <thead>
                  <tr>
                    <th scope="col">Client</th>
                    <th scope="col">Last seen</th>
                    <th scope="col" className="num">
                      Queries
                    </th>
                    <th scope="col" className="num">
                      Blocked
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {members.map((c) => (
                    <tr key={c.key}>
                      <td>
                        <Link href={clientHref(c.key)}>{c.name ?? c.key}</Link>
                        {c.name ? <span className="k-muted k-small k-mono"> {c.key}</span> : null}
                        {c.group && c.group !== ref ? <span className="k-muted k-small"> (resolved to {c.group.replace(/^default\//, '')})</span> : null}
                      </td>
                      <td>{c.last_seen ? <span title={c.last_seen}>{formatRelative(c.last_seen)}</span> : <span className="k-muted">–</span>}</td>
                      <td className="num">{formatExact(c.count)}</td>
                      <td className="num">{formatExact(c.blocked)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="k-muted">No client of this group sent a query in the last 24 hours.</p>
          )
        ) : null}
      </section>

      <section className="k-panel k-section" aria-labelledby="sec-pols">
        <div className="k-panel-title">
          <h2 id="sec-pols">Policies that apply</h2>
          <Link href="/policies/new">New policy</Link>
        </div>
        {applied.length ? (
          <>
            <ol className="k-policy-order" aria-label={`Policies of ${name}`}>
              {applied.map((p) => {
                const b = policyBadge(derivePolicyState(p, ctx));
                return (
                  <li key={p.metadata.name}>
                    <Link href={objectHref('policies', refOfObject(p))}>{p.metadata.name}</Link> <Badge variant={b.variant} label={b.label} />{' '}
                    <span className="k-muted k-small">
                      priority {p.spec.priority ?? 0} · {p.spec.schedule ? scheduleSummary(p.spec.schedule) : 'always'}
                    </span>
                  </li>
                );
              })}
            </ol>
            <p className="k-muted k-small">The first active policy decides the block response, upstream group and safe search; the lists of every active policy are merged.</p>
          </>
        ) : (
          <p className="k-muted">No policy names this group, so its members get the default policy.</p>
        )}
      </section>

      <YamlDialog obj={group} open={yaml} onClose={() => setYaml(false)} />
      <FormDialog
        isOpen={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete group ${name}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirmDelete(false)} />
            <Button label="Delete group" variant="destructive" isLoading={del.isPending} onClick={() => del.mutate()} />
          </>
        }
      >
        <p>
          Its members fall back to their other groups or the default group
          {applied.length ? `, and ${applied.map((p) => p.metadata.name).join(', ')} stop applying to them` : ''}. This cannot be undone.
        </p>
      </FormDialog>
    </div>
  );
}

function MatchersView({ matchers }: { matchers: ClientMatcher[] }) {
  if (!matchers.length) return <p className="k-muted">No matchers: the group matches nobody.</p>;
  return (
    <div className="k-table-wrap">
      <table className="k-table" aria-label="Matchers">
        <thead>
          <tr>
            <th scope="col">Kind</th>
            <th scope="col">Value</th>
            <th scope="col">Display name</th>
            <th scope="col">Comment</th>
          </tr>
        </thead>
        <tbody>
          {matchers.map((m, i) => {
            const kind = matcherKind(m);
            return (
              <tr key={i}>
                <td>
                  {kindLabel(kind)}
                  {kind === 'mac' || kind === 'hostname' || kind === 'nameGlob' ? (
                    <span title={kind === 'mac' ? 'Matches only when the transport supplies MAC addresses (inventory integrations: v0.2).' : 'No hostname source in this version: never matches.'}>
                      {' '}
                      <Badge variant="warning" label="limited" />
                    </span>
                  ) : null}
                </td>
                <td className="k-mono">{matcherValue(m)}</td>
                <td>{m.name ?? <span className="k-muted">–</span>}</td>
                <td className="k-small">{m.comment ?? ''}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
