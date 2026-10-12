// The entries of one source of a list, a page at a time, for every kind
// of source (GET /blocklists/{ns}/{name}/sources/{source}/entries): the
// lines of an inline source, the rules of the url or ConfigMap body this
// node fetched, or the structured allow/deny entries of the ui-* lists.
// Each entry has a switch (PATCH …/entries {enable, disable}: exclude[] of
// the source, or the entry's enabled flag), saved at once and moved back
// when the write fails; search, "Show disabled only", paging, bulk switch
// on/off; inline sources also add and remove (PATCH {add, remove}).
// Switching an entry off here affects only this list; the Allowlist is
// the way to allow a name everywhere.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { StackItem } from '@astryxdesign/core/Stack';
import { Switch } from '@astryxdesign/core/Switch';
import { pixel, proportional, Table, useTableSelection, useTableSelectionState, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Toolbar } from '@astryxdesign/core/Toolbar';
import { VStack } from '@astryxdesign/core/VStack';
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useState } from 'react';
import { ApiError } from '../../api/client';
import { qk, useApi } from '../../api/context';
import type { BlockList, ListEntriesPatch, ListEntry, SourceEntriesPage } from '../../api/types';
import { actionPath } from '../../lib/domains';
import { formatDateTime, formatExact, formatRelative } from '../../lib/format';
import { entriesProblem, listKey, listNamespace, parseEntries, sourceKind, sourceName } from '../../lib/lists';
import { EnumSelect } from '../fields/EnumSelect';
import { TextAreaField, TextField } from '../fields/TextField';
import { ErrorBanner, errorText, Loading, useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';

const PAGE_SIZES = [
  { value: '25', label: '25 per page' },
  { value: '50', label: '50 per page' },
  { value: '100', label: '100 per page' },
];

/** A ListEntry as a table row (a type literal, as the Table's Record constraint needs). */
type Row = { [K in keyof ListEntry]: ListEntry[K] } & { key: string };

const rowAttrs: TablePlugin<Row> = {
  transformBodyRow: (props, item) => ({
    ...props,
    htmlProps: { ...props.htmlProps, ...({ 'data-testid': `list-entry-${item.value}`, 'data-enabled': String(item.enabled) } as Record<string, string>) },
  }),
};

export interface EntriesBrowserProps {
  list: BlockList;
  index: number;
  editable: boolean;
  onClose: () => void;
}

function plural(n: number, one: string, many: string): string {
  return `${formatExact(n)} ${n === 1 ? one : many}`;
}

export function EntriesBrowser({ list, index, editable, onClose }: EntriesBrowserProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const titleId = useId();
  const ns = listNamespace(list);
  const name = list.metadata.name;
  const src = list.spec.sources[index];
  const source = src ? sourceName(index, src) : '';
  const specKind = src ? sourceKind(src) : 'inline';
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const [disabledOnly, setDisabledOnly] = useState(false);
  const [pageSize, setPageSize] = useState(25);
  // cursors[i] is the continue token of page i ("" for the first page).
  const [cursors, setCursors] = useState<string[]>(['']);
  const page = cursors.length - 1;
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [adding, setAdding] = useState('');
  const [confirm, setConfirm] = useState(false);
  const [rejected, setRejected] = useState<{ item: string; error: string }[]>([]);
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());

  // Debounced search; only a changed query goes back to the first page (the
  // mount-time run used to reset a page turned within the first 250 ms).
  useEffect(() => {
    const next = search.trim();
    if (next === query) return;
    const t = window.setTimeout(() => {
      setQuery(next);
      setCursors(['']);
      setSelected(new Set());
    }, 250);
    return () => window.clearTimeout(t);
  }, [search, query]);

  const cursor = cursors[page] ?? '';
  const entriesKey = [...listKey(ns, name), 'entries', source] as const;
  const pageKey = [...entriesKey, query, disabledOnly, pageSize, cursor];
  const q = useQuery({
    queryKey: pageKey,
    queryFn: () =>
      api.lists.entries(ns, name, source, {
        ...(query ? { q: query } : {}),
        ...(disabledOnly ? { enabled: 'false' as const } : {}),
        limit: pageSize,
        ...(cursor ? { continue: cursor } : {}),
      }),
    placeholderData: keepPreviousData,
    enabled: !!source,
    retry: false,
  });
  const data = q.data;
  const kind = data?.kind ?? specKind;
  const canAdd = editable && kind === 'inline';
  const notFetched = q.error instanceof ApiError && q.error.status === 503 ? q.error : null;

  const setEnabledInCache = (values: string[], enabled: boolean) =>
    qc.setQueriesData<SourceEntriesPage>({ queryKey: entriesKey }, (old) => (old ? { ...old, items: old.items.map((e) => (values.includes(e.value) ? { ...e, enabled } : e)) } : old));

  const patch = useMutation({
    mutationFn: (body: ListEntriesPatch) => api.lists.patchEntries(ns, name, source, body),
    // Switches move at once and move back when the write fails.
    onMutate: async (body) => {
      const values = [...(body.enable ?? []), ...(body.disable ?? [])];
      if (!values.length) return { snapshot: [] as [readonly unknown[], SourceEntriesPage | undefined][] };
      setPending((p) => new Set([...p, ...values]));
      await qc.cancelQueries({ queryKey: entriesKey });
      const snapshot = qc.getQueriesData<SourceEntriesPage>({ queryKey: entriesKey });
      if (body.enable?.length) setEnabledInCache(body.enable, true);
      if (body.disable?.length) setEnabledInCache(body.disable, false);
      return { snapshot };
    },
    onSuccess: (r, body) => {
      if (body.add || body.remove) {
        const errors = r?.processed?.errors ?? [];
        setRejected(errors);
        if (body.add) {
          const n = r?.added?.length ?? r?.processed?.success.length ?? body.add.length;
          if (n) notify.success(`Added ${plural(n, 'entry', 'entries')} to ${source}`);
          if (!errors.length) setAdding('');
        } else {
          const n = r?.removed?.length ?? r?.processed?.success.length ?? body.remove?.length ?? 0;
          notify.success(`Removed ${plural(n, 'entry', 'entries')} from ${source}`);
          setSelected(new Set());
          setConfirm(false);
        }
      } else {
        const on = r?.enabled ?? body.enable ?? [];
        const off = r?.disabled ?? body.disable ?? [];
        if (off.length) notify.success(off.length === 1 ? `${off[0] ?? ''} switched off in ${name}` : `Switched off ${plural(off.length, 'entry', 'entries')} in ${name}`);
        if (on.length) notify.success(on.length === 1 ? `${on[0] ?? ''} switched on in ${name}` : `Switched on ${plural(on.length, 'entry', 'entries')} in ${name}`);
        if ((body.enable?.length ?? 0) + (body.disable?.length ?? 0) > 1) setSelected(new Set());
      }
    },
    onError: (err, body, ctx) => {
      for (const [k, d] of ctx?.snapshot ?? []) qc.setQueryData(k, d);
      const what = body.add ? 'add the entries' : body.remove ? 'remove the entries' : body.enable?.length ? 'switch the entries on' : 'switch the entries off';
      notify.error(err, `Could not ${what}`);
    },
    onSettled: (_r, _e, body) => {
      const values = [...(body.enable ?? []), ...(body.disable ?? [])];
      if (values.length) {
        setPending((p) => {
          const next = new Set(p);
          for (const v of values) next.delete(v);
          return next;
        });
      }
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
  });

  const toAdd = parseEntries(adding);
  const addProblem = toAdd.length ? entriesProblem(toAdd, src?.format) : null;
  const rows: Row[] = (data?.items ?? []).map((e) => ({ ...e, key: e.value }));
  const chosen = rows.filter((r) => selected.has(r.key));
  const first = page * pageSize + 1;
  const ui = list.spec.mode === 'allow' ? 'allow' : 'deny';

  const { selectionConfig } = useTableSelectionState({ data: rows, idKey: 'key', selectedKeys: selected, setSelectedKeys: setSelected });
  const selection = useTableSelection({ ...selectionConfig, getRowLabel: (r: Row) => r.value });

  const summary = data
    ? [
        kind === 'inline' ? plural(data.total, 'inline entry', 'inline entries') : kind === 'entries' ? plural(data.total, `${ui} entry`, `${ui} entries`) : plural(data.total, 'rule', 'rules'),
        data.excluded ? `${formatExact(data.excluded)} switched off` : '',
        src?.format && src.format !== 'auto' ? `${src.format} format` : '',
      ]
        .filter(Boolean)
        .join(', ')
    : '';

  return (
    <VStack as="section" gap={4} className="k-panel" aria-labelledby={titleId} data-testid="entries-browser">
      <HStack gap={2} justify="between" align="start">
        <VStack gap={1}>
          <Heading level={2} id={titleId}>
            Entries of <Text type="code">{source}</Text>
          </Heading>
          <Text type="supporting" size="sm">
            {summary}
            {data?.fetched ? (
              <>
                {summary ? '; ' : ''}
                the copy this node fetched{' '}
                <time dateTime={data.fetched} title={formatDateTime(data.fetched)}>
                  {formatRelative(data.fetched)}
                </time>
              </>
            ) : null}
          </Text>
        </VStack>
        <Button label="Close entries" variant="ghost" onClick={onClose} />
      </HStack>

      {kind === 'entries' ? (
        <Text type="supporting">
          These are the{' '}
          <Link href={actionPath(ui)} hasUnderline>
            {ui === 'allow' ? 'Allowlist' : 'Denylist'}
          </Link>{' '}
          entries kept in {name}. Add, edit, scope and remove them there; the switches here and there are the same.
        </Text>
      ) : (
        <Text type="supporting">
          Switching off an entry of an imported list affects only this list; to allow a domain everywhere use the{' '}
          <Link href="/allowlist" hasUnderline>
            Allowlist
          </Link>
          .
        </Text>
      )}

      {canAdd ? (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (toAdd.length && !addProblem) patch.mutate({ add: toAdd });
          }}
        >
          <VStack gap={2}>
            <TextAreaField
              label="Add entries"
              help="One or many, one per line."
              placeholder="ads.example.com"
              rows={2}
              hasSpellCheck={false}
              value={adding}
              onChange={(v) => {
                setAdding(v);
                setRejected([]);
              }}
              error={addProblem ? `"${addProblem.first}": ${addProblem.error}` : undefined}
            />
            <HStack gap={2}>
              <Button type="submit" label={toAdd.length > 1 ? `Add ${toAdd.length}` : 'Add'} variant="secondary" isDisabled={!toAdd.length || !!addProblem} isLoading={patch.isPending && !!patch.variables?.add} />
            </HStack>
          </VStack>
        </form>
      ) : null}
      {rejected.length ? (
        <Banner
          status="warning"
          title={`${rejected.length} not added`}
          description={rejected
            .slice(0, 5)
            .map((e) => `${e.item}: ${e.error}`)
            .join('; ')}
        />
      ) : null}

      {data?.stale ? <Banner status="info" title="An older copy" description="This node still has an older copy of the source than the one the list status reports; it catches up at the next fetch." /> : null}
      {data?.staleExcludes.length ? (
        <Banner
          status="warning"
          title={`${plural(data.staleExcludes.length, 'switched-off entry is', 'switched-off entries are')} no longer in the source`}
          description="The source dropped them; they stay switched off in case they come back."
          endContent={
            editable ? (
              <Button label="Forget them" size="sm" variant="secondary" isLoading={patch.isPending && patch.variables?.enable === data.staleExcludes} onClick={() => patch.mutate({ enable: data.staleExcludes })} />
            ) : undefined
          }
          collapsible={{ defaultIsOpen: false }}
        >
          <List density="compact">
            {data.staleExcludes.slice(0, 20).map((v) => (
              <ListItem key={v} label={v} />
            ))}
          </List>
        </Banner>
      ) : null}

      <HStack gap={2} wrap="wrap" align="end">
        <TextField className="k-search" label="Search entries" startIcon="search" placeholder="Part of a domain or rule" value={search} onChange={setSearch} hasClear />
        <EnumSelect
          label="Page size"
          value={String(pageSize)}
          options={PAGE_SIZES}
          onChange={(v) => {
            setPageSize(Number(v));
            setCursors(['']);
          }}
        />
        <Switch
          label="Show disabled only"
          value={disabledOnly}
          onChange={(on) => {
            setDisabledOnly(on);
            setCursors(['']);
            setSelected(new Set());
          }}
        />
        <StackItem size="fill" />
      </HStack>

      {editable && selected.size > 0 ? (
        <Toolbar
          label="Bulk actions"
          size="sm"
          variant="muted"
          startContent={
            <>
              <Badge label={`${selected.size} selected`} />
              <Button label="Switch on" size="sm" variant="secondary" onClick={() => patch.mutate({ enable: chosen.filter((r) => !r.enabled).map((r) => r.value) })} isDisabled={!chosen.some((r) => !r.enabled)} />
              <Button label="Switch off" size="sm" variant="secondary" onClick={() => patch.mutate({ disable: chosen.filter((r) => r.enabled).map((r) => r.value) })} isDisabled={!chosen.some((r) => r.enabled)} />
              {canAdd ? <Button label={`Remove ${selected.size} selected`} size="sm" variant="destructive" onClick={() => setConfirm(true)} /> : null}
            </>
          }
          endContent={<Button label="Clear selection" size="sm" variant="ghost" onClick={() => setSelected(new Set())} />}
        />
      ) : null}

      {notFetched ? (
        <Banner status="info" title="This node has not fetched the list yet" description={typeof notFetched.hint === 'string' && notFetched.hint ? `${notFetched.message}. ${notFetched.hint}` : notFetched.message} />
      ) : q.error ? (
        <ErrorBanner error={q.error} title="Could not read the entries" />
      ) : null}
      {q.isLoading ? <Loading label="Loading entries" /> : null}
      {data ? (
        <Table<Row>
          aria-label={`Entries of ${source}`}
          density="compact"
          idKey="key"
          data={rows}
          plugins={editable ? { selection, rowAttrs } : { rowAttrs }}
          emptyState={
            <Text type="supporting">{query ? `No entry contains "${query}".` : disabledOnly ? 'No entry is switched off.' : 'No entries.'}</Text>
          }
          columns={[
            { key: 'line', header: 'Line', width: pixel(72), align: 'end', renderCell: (r) => <Text type="supporting">{r.line}</Text> },
            {
              key: 'value',
              header: 'Entry',
              width: proportional(4),
              renderCell: (r) => (
                <Text type="code" wordBreak="break-all" hasStrikethrough={!r.enabled}>
                  {r.value}
                </Text>
              ),
            },
            {
              key: 'enabled',
              header: 'Enabled',
              width: pixel(84),
              renderCell: (r) => (
                <Switch
                  label={`${r.value} enabled`}
                  isLabelHidden
                  size="sm"
                  value={r.enabled}
                  isLoading={pending.has(r.value)}
                  isDisabled={!editable || pending.has(r.value)}
                  disabledMessage={editable ? undefined : 'Read-only: the list is managed elsewhere or you lack blocklists.write'}
                  onChange={(on) => patch.mutate(on ? { enable: [r.value] } : { disable: [r.value] })}
                />
              ),
            },
          ]}
        />
      ) : null}

      <HStack gap={2} align="center" wrap="wrap">
        <Text type="supporting" size="sm" role="status">
          {rows.length
            ? `Showing ${formatExact(first)}–${formatExact(first + rows.length - 1)}${
                query ? ` of the entries containing "${query}"` : disabledOnly ? ` of ${formatExact(data?.matched)} switched off` : ` of ${formatExact(data?.total)}`
              }`
            : ''}
        </Text>
        <StackItem size="fill" />
        <Button label="Previous page" size="sm" variant="secondary" isDisabled={page === 0} onClick={() => setCursors((c) => c.slice(0, -1))} />
        <Button label="Next page" size="sm" variant="secondary" isDisabled={!data?.continue || q.isPlaceholderData} onClick={() => setCursors((c) => (data?.continue ? [...c, data.continue] : c))} />
      </HStack>

      <FormDialog
        isOpen={confirm}
        onClose={() => setConfirm(false)}
        title={`Remove ${plural(selected.size, 'entry', 'entries')}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(false)} />
            <Button label="Remove" variant="destructive" isLoading={patch.isPending} onClick={() => patch.mutate({ remove: [...selected] })} />
          </>
        }
      >
        <VStack gap={2}>
          <List density="compact">
            {[...selected].slice(0, 20).map((v) => (
              <ListItem key={v} label={v} />
            ))}
          </List>
          {selected.size > 20 ? <Text type="supporting">and {selected.size - 20} more</Text> : null}
          <Text type="supporting">They are removed from {source}; add them again to undo. To keep one for later, switch it off instead.</Text>
          {patch.error && patch.variables?.remove ? <Text type="supporting">{errorText(patch.error)}</Text> : null}
        </VStack>
      </FormDialog>
    </VStack>
  );
}
