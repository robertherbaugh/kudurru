// Clients seen by the query log in the last 24 hours (GET /clients: the
// top-clients ranking, keyed by IP or client ID), with the client group
// the identify stage resolved them to; a client opens its page. Selected
// clients can be added to a group in one step (a PUT of the ClientGroup
// with the new matchers, each with a display name and comment).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Code } from '@astryxdesign/core/Code';
import { Link } from '@astryxdesign/core/Link';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, ClientItem } from '../api/types';
import { matcherFor } from '../components/fields/MatchersField';
import { SelectField } from '../components/fields/SelectField';
import { TextField } from '../components/fields/TextField';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader } from '../components/layout/Page';
import { ScopeNote } from '../components/ScopeSelector';
import { formatExact, formatRelative } from '../lib/format';
import { objectOwnership } from '../lib/managedBy';
import { clientHref, objectHref } from '../lib/policies';
import { useSearchParams } from '../lib/router';

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

  const inv = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list(scope, { window: '24h' }), refetchInterval: 60_000 });
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

  const columns: TableColumn<ClientRow>[] = [
    {
      key: 'select',
      header: <VisuallyHidden>Select</VisuallyHidden>,
      width: pixel(44),
      renderCell: (c) => <CheckboxInput label={`Select ${c.name ?? c.key}`} isLabelHidden value={selected.has(c.key)} onChange={(on) => toggle(c, on)} />,
    },
    {
      key: 'key',
      header: 'Client',
      width: proportional(3, { minWidth: 180 }),
      renderCell: (c) => (
        <Text>
          <Link href={clientHref(c.key)} type="inherit" color="inherit" hasUnderline>
            {c.name ?? c.key}
          </Link>
          {c.name ? (
            <Text type="supporting">
              {' '}
              <Code color="inherit" size="inherit">
                {c.key}
              </Code>
            </Text>
          ) : null}
        </Text>
      ),
    },
    {
      key: 'group',
      header: 'Group',
      width: proportional(1.5, { minWidth: 110 }),
      renderCell: (c) =>
        c.group ? (
          <Link href={objectHref('groups', c.group)}>
            <Badge variant={groupName(c.group) === 'default' ? 'neutral' : 'blue'} label={c.group.replace(/^default\//, '')} />
          </Link>
        ) : (
          <Text color="secondary">–</Text>
        ),
    },
    {
      key: 'last_seen',
      header: 'Last seen',
      width: proportional(1.5, { minWidth: 110 }),
      renderCell: (c) =>
        c.last_seen ? (
          <Tooltip content={c.last_seen}>
            <Text>{formatRelative(c.last_seen)}</Text>
          </Tooltip>
        ) : (
          <Text color="secondary">–</Text>
        ),
    },
    {
      key: 'count',
      header: 'Queries (24 h)',
      align: 'end',
      width: proportional(2.2, { minWidth: 160 }),
      renderCell: (c) => (
        <Link href={`/queries?client=${encodeURIComponent(c.key)}`} color="inherit" hasUnderline>
          <Text type="inherit" hasTabularNumbers>
            {formatExact(c.count)}
          </Text>
        </Link>
      ),
    },
    {
      key: 'blocked',
      header: 'Blocked',
      align: 'end',
      renderCell: (c) => (
        <Link href={`/queries?client=${encodeURIComponent(c.key)}&status_class=blocked`} color="inherit" hasUnderline>
          <Text type="inherit" hasTabularNumbers>
            {formatExact(c.blocked)}
          </Text>
        </Link>
      ),
    },
  ];

  return (
    <Page>
      <PageHeader
        title="Clients"
        description="Every client seen in the last 24 hours, the client group it resolved to, and its query counts."
        actions={
          <>
            {inv.data?.scope ? <ScopeNote scope={inv.data} /> : null}
            <Button
              label={`Add ${selected.size || ''} to group`.replace('  ', ' ')}
              variant="primary"
              isDisabled={selected.size === 0 || !can('clientgroups', 'write')}
              onClick={() => {
                setGroup(writable[0]?.metadata.name ?? '');
                setAssigning(true);
              }}
            />
          </>
        }
      />
      <TextField label="Search clients" isLabelHidden startIcon="search" placeholder="Search by address, name or group" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
      {inv.error ? <ErrorBanner error={inv.error} /> : null}
      {inv.isLoading ? <Loading /> : null}
      {inv.data ? (
        <Card padding={0}>
          <Table<ClientRow>
            aria-label="Clients"
            density="compact"
            hasHover
            idKey="key"
            data={items}
            columns={columns}
            emptyState={<Text color="secondary">{search ? `No client matches "${search}".` : 'No queries in the last 24 hours.'}</Text>}
          />
        </Card>
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
        <SelectField label="Group" value={group} options={writable.map((g) => ({ value: g.metadata.name, label: g.metadata.name }))} onChange={setGroup} />
        <TextField label="Comment" value={comment} onChange={setComment} isOptional />
      </FormDialog>
    </Page>
  );
}

type ClientRow = ClientItem & Record<string, unknown>;
