// The page body of /allowlist, /denylist (one action each) and /domains
// (both): filters kept in the URL (q, type, enabled, scope, and action on
// the combined page), a page of entries at a time with continue tokens,
// the entries table with its switches and row actions, bulk enable,
// disable and delete (POST …:batch), the add / edit / delete dialogs, the
// lists behind the entries and a warning for policies that should use
// those lists but are managed elsewhere (`unattached`).
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { StackItem } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Toolbar } from '@astryxdesign/core/Toolbar';
import { VStack } from '@astryxdesign/core/VStack';
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../../api/context';
import type { ClientGroup, DomainAction, DomainEntriesPage, DomainEntriesQuery, DomainEntry, DomainListInfo, DomainUnattached } from '../../api/types';
import { ACTION_LABEL, ACTION_OPTIONS, actionPath, ENABLED_OPTIONS, entriesWord, refField, refPath, shortRef, TYPE_OPTIONS, uiListAction } from '../../lib/domains';
import { formatExact } from '../../lib/format';
import { useSearchParams } from '../../lib/router';
import { SelectField, type SelectOption } from '../fields/SelectField';
import { TextField } from '../fields/TextField';
import { ErrorBanner, Loading, useNotify } from '../Feedback';
import { DomainCheck } from './DomainCheck';
import { DomainEntriesTable } from './DomainEntriesTable';
import { AddEntryDialog, DeleteEntriesDialog, EditEntryDialog, useInvalidateDomains, writeSummary } from './DomainEntryDialogs';

const PAGE = 50;

export interface DomainEntriesViewProps {
  /** undefined: the combined /domains page. */
  action?: DomainAction;
}

const TITLE = { allow: 'Allowlist', deny: 'Denylist', all: 'Domains' };
const INTRO = {
  allow: 'Domains and expressions that are never blocked, whatever the block lists say: for everyone or for some client groups. They live in the ui-allow lists, which every policy the dashboard manages uses.',
  deny: 'Domains and expressions blocked on top of the block lists: for everyone or for some client groups. They live in the ui-deny lists. An allow entry for the same name still wins.',
  all: 'Every allow and deny entry in one table, like Pi-hole’s Domains page. Big imported lists are on the Lists page; switch off single entries of those on the list itself.',
};

/** The sentence for a policy that should use a list but is managed elsewhere. */
export function unattachedText(u: DomainUnattached): string {
  const action = uiListAction(shortRef(u.list).replace(/^.*\//, '')) ?? 'allow';
  return `Policy ${shortRef(u.policy)} is managed by ${u.managedBy}: add ${shortRef(u.list)} to its ${refField(action)} so these entries apply to its clients.`;
}

export function DomainEntriesView({ action }: DomainEntriesViewProps) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const invalidate = useInvalidateDomains();
  const canWrite = can('blocklists', 'write');
  const [sp, setParams] = useSearchParams();
  const key = action ?? 'all';

  const filters = useMemo(() => {
    const f: DomainEntriesQuery = {};
    const type = sp.get('type');
    if (type === 'exact' || type === 'wildcard' || type === 'regex') f.type = type;
    const enabled = sp.get('enabled');
    if (enabled === 'true' || enabled === 'false') f.enabled = enabled;
    const scope = sp.get('scope');
    if (scope) f.scope = scope;
    const q = sp.get('q');
    if (q) f.q = q;
    const a = sp.get('action');
    if (!action && (a === 'allow' || a === 'deny')) f.action = a;
    return f;
  }, [sp, action]);
  const filterKey = JSON.stringify(filters);

  // The search box writes ?q= after a pause, so typing does not query per key.
  const [search, setSearch] = useState(filters.q ?? '');
  useEffect(() => {
    const next = search.trim();
    if (next === (filters.q ?? '')) return;
    const t = window.setTimeout(() => setParams({ q: next || null }), 250);
    return () => window.clearTimeout(t);
  }, [search, filters.q, setParams]);

  // cursors[i] is the continue token of page i; a filter change starts over.
  const [cursors, setCursors] = useState<string[]>(['']);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [seenFilters, setSeenFilters] = useState(filterKey);
  if (seenFilters !== filterKey) {
    setSeenFilters(filterKey);
    setCursors(['']);
    setSelected(new Set());
  }
  const page = cursors.length - 1;
  const cursor = cursors[page] ?? '';

  const q = useQuery({
    queryKey: qk.domains(key, filters, cursor),
    queryFn: () => api.domains.list(action, { ...filters, limit: PAGE, ...(cursor ? { continue: cursor } : {}) }),
    placeholderData: keepPreviousData,
  });
  const groups = useQuery({ queryKey: qk.objects('clientgroups'), queryFn: () => api.objects.list<ClientGroup>('clientgroups') });

  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<DomainEntry | null>(null);
  const [deleting, setDeleting] = useState<DomainEntry[] | null>(null);
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());

  const items = q.data?.items ?? [];
  const chosen = items.filter((e) => selected.has(e.id));

  /** Replaces entries in every cached page (optimistic updates and write answers). */
  const patchCache = (fn: (e: DomainEntry) => DomainEntry) =>
    qc.setQueriesData<DomainEntriesPage>({ queryKey: qk.domains() }, (old) => (old ? { ...old, items: old.items.map(fn) } : old));

  const toggle = useMutation({
    mutationFn: (v: { entry: DomainEntry; enabled: boolean }) => api.domains.patch(v.entry.action, v.entry.id, { enabled: v.enabled }),
    // Optimistic: the switch moves at once and moves back when the write fails.
    onMutate: async (v) => {
      setPending((p) => new Set(p).add(v.entry.id));
      await qc.cancelQueries({ queryKey: qk.domains() });
      const snapshot = qc.getQueriesData<DomainEntriesPage>({ queryKey: qk.domains() });
      patchCache((e) => (e.id === v.entry.id ? { ...e, enabled: v.enabled } : e));
      return { snapshot };
    },
    onError: (err, v, ctx) => {
      for (const [k, data] of ctx?.snapshot ?? []) qc.setQueryData(k, data);
      notify.error(err, `Could not ${v.enabled ? 'enable' : 'disable'} ${v.entry.value}`);
    },
    onSuccess: (r, v) => {
      const saved = r.items.find((e) => e.id === v.entry.id);
      if (saved) patchCache((e) => (e.id === saved.id ? saved : e));
      notify.success(writeSummary(`${v.entry.value} ${v.enabled ? 'enabled' : 'disabled'}.`, r, v.entry.action));
    },
    onSettled: (_r, _e, v) => {
      setPending((p) => {
        const next = new Set(p);
        next.delete(v.entry.id);
        return next;
      });
      invalidate();
    },
  });

  const comment = async (entry: DomainEntry, text: string) => {
    try {
      const r = await api.domains.patch(entry.action, entry.id, { comment: text });
      const saved = r.items.find((e) => e.id === entry.id);
      if (saved) patchCache((e) => (e.id === saved.id ? saved : e));
      notify.success(text ? `Comment saved for ${entry.value}` : `Comment removed from ${entry.value}`);
      invalidate();
    } catch (err) {
      notify.error(err, `Could not save the comment of ${entry.value}`);
      throw err;
    }
  };

  /** One batch per action (the combined page may hold both). */
  const batch = useMutation({
    mutationFn: async (v: { entries: DomainEntry[]; op: 'enable' | 'disable' | 'delete' }) => {
      const byAction = new Map<DomainAction, string[]>();
      for (const e of v.entries) byAction.set(e.action, [...(byAction.get(e.action) ?? []), e.id]);
      const results = [];
      for (const [a, ids] of byAction) {
        if (v.op === 'delete' && ids.length === 1 && ids[0]) {
          await api.domains.delete(a, ids[0]);
          results.push({ action: a, skipped: [] as string[], attached: [] as string[], errors: [] as { item: string; error: string }[] });
        } else {
          const r = await api.domains.batch(a, { ids, op: v.op });
          results.push({ action: a, skipped: r.skipped ?? [], attached: r.attached ?? [], errors: r.processed?.errors ?? [] });
        }
      }
      return results;
    },
    onSuccess: (results, v) => {
      const failed = results.flatMap((r) => r.errors);
      const done = v.entries.length - failed.length;
      const verb = v.op === 'delete' ? 'Deleted' : v.op === 'enable' ? 'Enabled' : 'Disabled';
      const first = results[0];
      if (done) notify.success(writeSummary(`${verb} ${done} ${entriesWord(done)}.`, { attached: results.flatMap((r) => r.attached), skipped: results.flatMap((r) => r.skipped) }, first?.action ?? 'allow'));
      for (const f of failed) notify.error(new Error(`${f.item}: ${f.error}`));
      setSelected(new Set());
      setDeleting(null);
      invalidate();
    },
    onError: (err) => notify.error(err, 'Could not change the entries'),
  });

  const scopeOptions: SelectOption[] = [
    { value: 'everyone', label: 'Everyone' },
    ...(groups.data?.items ?? []).map((g) => {
      const ref = `${g.metadata.namespace ?? 'default'}/${g.metadata.name}`;
      return { value: ref, label: shortRef(ref) };
    }),
  ];
  const anyFilter = Object.keys(filters).length > 0;
  const lists = q.data?.lists ?? [];
  const unattached = q.data?.unattached ?? [];
  const offLists = lists.filter((l) => !l.enabled);
  const first = page * PAGE + 1;
  const writeTip = canWrite ? undefined : 'Needs blocklists.write';

  return (
    <VStack gap={4} className="k-page">
      <HStack gap={3} justify="between" align="start" wrap="wrap">
        <VStack gap={1}>
          <Heading level={1}>{TITLE[key]}</Heading>
          <Text type="supporting">{INTRO[key]}</Text>
          <HStack gap={3} wrap="wrap">
            {action ? (
              <Link href="/domains" size="sm">
                Allow and deny entries together
              </Link>
            ) : (
              <>
                <Link href="/allowlist" size="sm">
                  Allowlist only
                </Link>
                <Link href="/denylist" size="sm">
                  Denylist only
                </Link>
              </>
            )}
            {action ? (
              <Link href={actionPath(action === 'allow' ? 'deny' : 'allow')} size="sm">
                {ACTION_LABEL[action === 'allow' ? 'deny' : 'allow']}
              </Link>
            ) : null}
          </HStack>
        </VStack>
        <Button label={action ? `Add to the ${ACTION_LABEL[action]}` : 'Add domains'} variant="primary" isDisabled={!canWrite} tooltip={writeTip} onClick={() => setAdding(true)} />
      </HStack>

      {unattached.map((u) => (
        <Banner key={`${u.list}|${u.policy}`} status="warning" title={`Not applied for ${shortRef(u.policy)}`} description={unattachedText(u)} />
      ))}
      {offLists.map((l) => (
        <Banner
          key={l.list}
          status="warning"
          title={`${shortRef(l.list)} is turned off`}
          description="Its entries are kept but do not apply until the list is turned on again on its page."
          endContent={
            <Link href={refPath('lists', l.list)} size="sm">
              Open {shortRef(l.list)}
            </Link>
          }
        />
      ))}

      <HStack gap={2} wrap="wrap" align="end">
        <TextField className="k-search" label="Search" isLabelHidden startIcon="search" placeholder="Search domains, expressions and comments" value={search} onChange={setSearch} hasClear />
        {!action ? <SelectField label="Action" isLabelHidden value={filters.action ?? ''} options={ACTION_OPTIONS} emptyLabel="Allow and deny" onChange={(v) => setParams({ action: v || null })} /> : null}
        <SelectField label="Type" isLabelHidden value={filters.type ?? ''} options={TYPE_OPTIONS} emptyLabel="All types" onChange={(v) => setParams({ type: v || null })} />
        <SelectField label="Enabled" isLabelHidden value={filters.enabled ?? ''} options={ENABLED_OPTIONS} emptyLabel="Enabled and disabled" onChange={(v) => setParams({ enabled: v || null })} />
        <SelectField label="Applies to" isLabelHidden value={filters.scope ?? ''} options={scopeOptions} emptyLabel="Everyone and every group" onChange={(v) => setParams({ scope: v || null })} />
        {anyFilter ? (
          <Button
            label="Clear filters"
            variant="ghost"
            onClick={() => {
              setSearch('');
              setParams({ q: null, type: null, enabled: null, scope: null, action: null });
            }}
          />
        ) : null}
      </HStack>

      {selected.size > 0 ? (
        <Toolbar
          label="Bulk actions"
          size="sm"
          variant="muted"
          startContent={
            <>
              <Badge label={`${selected.size} selected`} />
              <Button label="Enable" size="sm" variant="secondary" isLoading={batch.isPending && batch.variables?.op === 'enable'} onClick={() => batch.mutate({ entries: chosen, op: 'enable' })} />
              <Button label="Disable" size="sm" variant="secondary" isLoading={batch.isPending && batch.variables?.op === 'disable'} onClick={() => batch.mutate({ entries: chosen, op: 'disable' })} />
              <Button label="Delete" size="sm" variant="destructive" onClick={() => setDeleting(chosen)} />
            </>
          }
          endContent={<Button label="Clear selection" size="sm" variant="ghost" onClick={() => setSelected(new Set())} />}
        />
      ) : null}

      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading label="Loading entries" /> : null}
      {q.data ? (
        <DomainEntriesTable
          entries={items}
          showAction={!action}
          canWrite={canWrite}
          selected={selected}
          setSelected={setSelected}
          pending={pending}
          onToggle={(entry, on) => toggle.mutate({ entry, enabled: on })}
          onComment={comment}
          onEdit={setEditing}
          onDelete={(e) => setDeleting([e])}
          label={TITLE[key]}
          emptyText={anyFilter ? 'No entry matches the filters.' : action ? `The ${ACTION_LABEL[action]} is empty. Add a domain, or use Allow and Deny in the query log.` : 'No allow or deny entries yet.'}
        />
      ) : null}

      {q.data ? (
        <HStack gap={2} align="center" wrap="wrap">
          <Text type="supporting" size="sm" role="status">
            {items.length
              ? `Showing ${formatExact(first)}–${formatExact(first + items.length - 1)} of ${formatExact(q.data.matched)}${anyFilter ? ` matching (${formatExact(q.data.total)} in total)` : ''}`
              : ''}
          </Text>
          <StackItem size="fill" />
          <Button label="Previous page" size="sm" variant="secondary" isDisabled={page === 0} onClick={() => setCursors((c) => c.slice(0, -1))} />
          <Button label="Next page" size="sm" variant="secondary" isDisabled={!q.data.continue || q.isPlaceholderData} onClick={() => setCursors((c) => (q.data?.continue ? [...c, q.data.continue] : c))} />
        </HStack>
      ) : null}

      {lists.length ? <ListsLine lists={lists} /> : null}

      <DomainCheck />

      <AddEntryDialog
        key={`${key}-${adding ? 'open' : 'closed'}`}
        open={adding}
        onClose={() => setAdding(false)}
        {...(action ? { action } : { defaultAction: filters.action ?? 'deny' })}
        defaultType={filters.type ?? 'exact'}
      />
      {editing ? <EditEntryDialog key={editing.id} entry={editing} onClose={() => setEditing(null)} /> : null}
      {deleting ? <DeleteEntriesDialog entries={deleting} busy={batch.isPending} onClose={() => setDeleting(null)} onConfirm={() => batch.mutate({ entries: deleting, op: 'delete' })} /> : null}
    </VStack>
  );
}

/** "Stored in ui-allow (12 entries, used by default) · ui-allow-kids (…)". */
function ListsLine({ lists }: { lists: DomainListInfo[] }) {
  return (
    <Text type="supporting" size="sm" data-testid="domain-lists">
      Stored in{' '}
      {lists.map((l, i) => (
        <Text key={l.list} type="inherit">
          {i ? ' · ' : ''}
          <Link href={refPath('lists', l.list)} size="sm">
            {shortRef(l.list)}
          </Link>{' '}
          ({formatExact(l.entries)} {entriesWord(l.entries)}
          {l.group ? `, group ${shortRef(l.group)}` : ''}
          {l.policies.length ? `, used by ${l.policies.map((p) => shortRef(p)).join(', ')}` : ', used by no policy'}
          {l.enabled ? '' : ', turned off'})
        </Text>
      ))}
      . Switching off an entry of an imported block list is done on that list&rsquo;s page and affects only that list.
    </Text>
  );
}
