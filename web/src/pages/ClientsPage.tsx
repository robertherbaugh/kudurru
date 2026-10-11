// Clients seen by the query log in the last 24 hours (GET /clients: the
// top-clients ranking, keyed by IP or client ID), with the client group
// the identify stage resolved them to; a client opens its page. Selected
// clients can be added to a group in one step (a PUT of the ClientGroup
// with the new matchers, each with a display name and comment).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useMemo, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, ClientItem } from '../api/types';
import { matcherFor } from '../components/fields/MatchersField';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { formatExact, formatRelative } from '../lib/format';
import { objectOwnership } from '../lib/managedBy';
import { clientHref, objectHref } from '../lib/policies';
import { Link, useSearchParams } from '../lib/router';

export { matcherFor };

/** The ClientGroup name of a "namespace/name" group reference. */
function groupName(ref: string): string {
  const i = ref.indexOf('/');
  return i >= 0 ? ref.slice(i + 1) : ref;
}

export default function ClientsPage() {
  const api = useApi();
  const can = useCan();
  const scope = useScopeParams();
  const qc = useQueryClient();
  const notify = useNotify();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [assigning, setAssigning] = useState(false);
  const [group, setGroup] = useState('');
  const [comment, setComment] = useState('');
  const groupId = useId();

  const inv = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list({ window: '24h' }), refetchInterval: 60_000 });
  const groups = useQuery({ queryKey: qk.objects('clientgroups'), queryFn: () => api.objects.list<ClientGroup>('clientgroups') });
  const writable = (groups.data?.items ?? []).filter((g) => objectOwnership(g).editable);

  const items = useMemo(() => {
    const s = search.trim().toLowerCase();
    const all = inv.data?.items ?? [];
    return s ? all.filter((c) => c.key.toLowerCase().includes(s) || (c.name ?? '').toLowerCase().includes(s) || (c.group ?? '').toLowerCase().includes(s)) : all;
  }, [inv.data, search]);

  const assign = useMutation({
    mutationFn: async () => {
      const g = writable.find((x) => x.metadata.name === group);
      if (!g) throw new Error('Pick a group');
      const chosen = (inv.data?.items ?? []).filter((c) => selected.has(c.key));
      const existing = new Set((g.spec.clients ?? []).flatMap((m) => [m.ip, m.clientID]).filter(Boolean));
      const add = chosen.filter((c) => !existing.has(c.key)).map((c) => matcherFor(c, comment.trim()));
      await api.objects.put('clientgroups', { ...g, spec: { ...g.spec, clients: [...(g.spec.clients ?? []), ...add] } });
      return add.length;
    },
    onSuccess: (n) => {
      notify.success(`Added ${n} client${n === 1 ? '' : 's'} to ${group}`);
      setAssigning(false);
      setSelected(new Set());
      setComment('');
      void qc.invalidateQueries({ queryKey: qk.objects('clientgroups') });
      void qc.invalidateQueries({ queryKey: qk.clients() });
    },
    onError: (err) => notify.error(err, 'Could not update the group'),
  });

  const toggle = (c: ClientItem, on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(c.key);
      else next.delete(c.key);
      return next;
    });

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Clients</h1>
          <p className="k-muted k-small">Every client seen in the last 24 hours, the client group it resolved to, and its query counts.</p>
        </div>
        <div className="k-actions">
          <Button
            label={`Add ${selected.size || ''} to group`.replace('  ', ' ')}
            variant="primary"
            isDisabled={selected.size === 0 || !can('clientgroups', 'write')}
            onClick={() => {
              setGroup(writable[0]?.metadata.name ?? '');
              setAssigning(true);
            }}
          />
        </div>
      </div>
      <TextInput label="Search clients" isLabelHidden placeholder="Search by address, name or group" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
      {inv.error ? <ErrorBanner error={inv.error} /> : null}
      {inv.isLoading ? <Loading /> : null}
      {inv.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Clients">
            <thead>
              <tr>
                <th scope="col" className="k-col-check">
                  <span className="k-visually-hidden">Select</span>
                </th>
                <th scope="col">Client</th>
                <th scope="col">Group</th>
                <th scope="col">Last seen</th>
                <th scope="col" className="num">
                  Queries (24 h)
                </th>
                <th scope="col" className="num">
                  Blocked
                </th>
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td colSpan={6} className="k-muted">
                    {search ? `No client matches "${search}".` : 'No queries in the last 24 hours.'}
                  </td>
                </tr>
              ) : null}
              {items.map((c) => (
                <tr key={c.key}>
                  <td className="k-col-check">
                    <CheckboxInput label={`Select ${c.name ?? c.key}`} isLabelHidden value={selected.has(c.key)} onChange={(on) => toggle(c, on)} />
                  </td>
                  <td>
                    <Link href={clientHref(c.key)}>{c.name ?? c.key}</Link>
                    {c.name ? <span className="k-muted k-small k-mono"> {c.key}</span> : null}
                  </td>
                  <td>
                    {c.group ? (
                      <Link href={objectHref('groups', c.group)} className="k-badge-link">
                        <Badge variant={groupName(c.group) === 'default' ? 'neutral' : 'blue'} label={c.group.replace(/^default\//, '')} />
                      </Link>
                    ) : (
                      <span className="k-muted">–</span>
                    )}
                  </td>
                  <td>{c.last_seen ? <span title={c.last_seen}>{formatRelative(c.last_seen)}</span> : <span className="k-muted">–</span>}</td>
                  <td className="num">
                    <Link href={`/queries?client=${encodeURIComponent(c.key)}`}>{formatExact(c.count)}</Link>
                  </td>
                  <td className="num">
                    <Link href={`/queries?client=${encodeURIComponent(c.key)}&status_class=blocked`}>{formatExact(c.blocked)}</Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <FormDialog
        isOpen={assigning}
        onClose={() => setAssigning(false)}
        title={`Add ${selected.size} client${selected.size === 1 ? '' : 's'} to a group`}
        width={460}
        onSubmit={() => assign.mutate()}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setAssigning(false)} />
            <Button type="submit" label="Add" variant="primary" isLoading={assign.isPending} isDisabled={!group} />
          </>
        }
      >
        <label className="k-label" htmlFor={groupId}>
          Group
          <select id={groupId} className="k-native-select" value={group} onChange={(e) => setGroup(e.target.value)}>
            {writable.map((g) => (
              <option key={g.metadata.name} value={g.metadata.name}>
                {g.metadata.name}
              </option>
            ))}
          </select>
        </label>
        <TextInput label="Comment" value={comment} onChange={setComment} isOptional />
      </FormDialog>
    </div>
  );
}
