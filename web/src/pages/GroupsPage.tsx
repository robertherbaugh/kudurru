// Client groups: who is who. The bootstrap "default" group matches every
// client, so new devices are auto-assigned to it until a more specific
// group claims them. The list shows each group's matchers, the members the
// controller counted and the policies that apply, in priority order; a row
// opens the group's page, "New group" the wizard.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMemo } from 'react';
import { useCan } from '../api/context';
import { ErrorBanner, Loading } from '../components/Feedback';
import { describeMatcher } from '../components/fields/MatchersField';
import { ManagedBadge } from '../components/ManagedBadge';
import { formatExact } from '../lib/format';
import { MANAGED_BY, objectComment, objectEnabled } from '../lib/managedBy';
import { objectHref, policiesOfGroup, refOfObject } from '../lib/policies';
import { Link, useNavigate, useSearchParams } from '../lib/router';
import { useGroups, usePolicies } from '../lib/usePolicyData';

export default function GroupsPage() {
  const can = useCan();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const groups = useGroups();
  const pols = usePolicies();
  const items = useMemo(() => {
    const s = search.trim().toLowerCase();
    const all = groups.data?.items ?? [];
    return s
      ? all.filter((g) => g.metadata.name.toLowerCase().includes(s) || objectComment(g).toLowerCase().includes(s) || (g.spec.clients ?? []).some((m) => describeMatcher(m).toLowerCase().includes(s)))
      : all;
  }, [groups.data, search]);
  const canWrite = can('clientgroups', 'write');

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Groups</h1>
          <p className="k-muted k-small">Client groups match devices by IP address, CIDR range, client ID or MAC address. Clients no other group claims fall into "default".</p>
        </div>
        <div className="k-actions">
          <Button label="New group" variant="primary" isDisabled={!canWrite} tooltip={canWrite ? undefined : 'Needs clientgroups.write'} onClick={() => navigate('/groups/new')} />
        </div>
      </div>
      <TextInput className="k-search" label="Search groups" isLabelHidden placeholder="Search by name, comment or member" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
      {groups.error ? <ErrorBanner error={groups.error} /> : null}
      {groups.isLoading ? <Loading /> : null}
      {groups.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Groups">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Members</th>
                <th scope="col" className="num">
                  Seen
                </th>
                <th scope="col">Policies</th>
                <th scope="col">Managed by</th>
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td colSpan={5} className="k-muted">
                    {search ? `No group matches "${search}".` : 'No groups yet.'}
                  </td>
                </tr>
              ) : null}
              {items.map((g) => {
                const list = g.spec.clients ?? [];
                const applied = policiesOfGroup(g, pols.data?.items ?? []);
                const comment = objectComment(g);
                return (
                  <tr key={refOfObject(g)} data-testid={`row-${g.metadata.name}`}>
                    <td>
                      <Link href={objectHref('groups', refOfObject(g))}>{g.metadata.name}</Link>
                      {objectEnabled(g) ? null : (
                        <>
                          {' '}
                          <Badge variant="neutral" label="disabled" />
                        </>
                      )}
                      {comment ? <div className="k-muted k-small">{comment}</div> : null}
                    </td>
                    <td className="k-small k-break">
                      {list.slice(0, 4).map(describeMatcher).join(', ') || <span className="k-muted">none</span>}
                      {list.length > 4 ? ` and ${list.length - 4} more` : ''}
                      {g.spec.sources?.integrationRef ? ` + integration ${g.spec.sources.integrationRef.name}` : ''}
                    </td>
                    <td className="num">{formatExact(g.status?.members)}</td>
                    <td className="k-small">
                      {applied.length ? (
                        applied.map((p, i) => (
                          <span key={p.metadata.name}>
                            {i ? ', ' : ''}
                            <Link href={objectHref('policies', refOfObject(p))}>{p.metadata.name}</Link>
                          </span>
                        ))
                      ) : (
                        <span className="k-muted">default policy</span>
                      )}
                    </td>
                    <td>
                      <ManagedBadge owner={g.metadata.annotations?.[MANAGED_BY]} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}
