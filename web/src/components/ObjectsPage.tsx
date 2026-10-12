// The generic object list page (Lists, Groups, Policies; §10 principle 6):
// search, enabled switches and comments on every row, "managed by" badges,
// multi-select with :batchDelete, and the generated create/edit form in a
// dialog. Objects managed elsewhere are listed but not selectable and open
// read-only. Kinds with their own page and wizard (all three) pass detailPath
// and wizard: names link to the page and "New" opens the wizard in a modal
// over the list (?new=1, so /policies?new=1 and /policies/new open it too).
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Switch } from '@astryxdesign/core/Switch';
import { proportional, Table, useTableSelection, useTableSelectionState, type ColumnWidth, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState, type ComponentType, type ReactNode } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { AnyObject } from '../api/types';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, withMeta } from '../lib/managedBy';
import type { KindSchema } from '../lib/objectSchemas';
import { useNewParam, useSearchParams } from '../lib/router';
import { ErrorBanner, Loading, useNotify } from './Feedback';
import { TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';
import { Page, PageHeader } from './layout/Page';
import { ManagedBadge } from './ManagedBadge';
import { ObjectForm } from './ObjectForm';

export interface Column<T> {
  header: string;
  cell: (obj: T) => ReactNode;
  /** Horizontal alignment of the column (numbers: 'end'). */
  align?: 'start' | 'center' | 'end';
  /** Column width, e.g. proportional(2) for a wide column (default: proportional(1); 0.75 for numbers). */
  width?: ColumnWidth;
  /**
   * @deprecated The table is an Astryx Table now; use `align`. Only `num`
   * still has an effect (it aligns the column to the end).
   */
  className?: string;
}

/** A table row: the object and its selection key. */
interface Row<T> extends Record<string, unknown> {
  id: string;
  obj: T;
}

/** Each body row carries data-testid="row-<name>" (tests and e2e find rows by it). */
const rowAttrs: TablePlugin<Row<AnyObject>> = {
  transformBodyRow: (props, item) => ({
    ...props,
    htmlProps: { ...props.htmlProps, ...({ 'data-testid': `row-${item.obj.metadata.name}` } as Record<string, string>) },
  }),
};

export interface ObjectsPageProps<T extends AnyObject> {
  title: string;
  description: ReactNode;
  schema: KindSchema;
  columns: Column<T>[];
  /** Extra buttons in the page header (multi-add, search…). */
  headerActions?: ReactNode;
  /** Extra per-row actions (refresh…). */
  rowActions?: (obj: T) => ReactNode;
  /** Content between the header and the table. */
  children?: ReactNode;
  /** Called after a create or edit was saved. */
  onSaved?: (obj: AnyObject) => void;
  /** The object's own page: the name links there instead of opening the form dialog. */
  detailPath?: (obj: T) => string;
  /** The creation wizard: "New" opens it (?new=1) instead of the form dialog. */
  wizard?: ComponentType<{ isOpen: boolean; onClose: () => void }>;
  /** Row order (default: the server's, by name). */
  sort?: (a: T, b: T) => number;
  /** Why "New" cannot be used right now (a license limit); disables the button. */
  createBlocked?: string;
}

function key(o: AnyObject): string {
  return `${o.metadata.namespace ?? ''}/${o.metadata.name}`;
}

export function ObjectsPage<T extends AnyObject>({ title, description, schema, columns, headerActions, rowActions, children, onSaved, detailPath, wizard: CreateWizard, sort, createBlocked }: ObjectsPageProps<T>) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const [editing, setEditing] = useState<T | 'new' | null>(null);
  const [creating, setCreating] = useNewParam();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirm, setConfirm] = useState(false);
  const canWrite = can(schema.resource, 'write');
  const canDelete = can(schema.resource, 'delete');

  const q = useQuery({ queryKey: qk.objects(schema.kind), queryFn: () => api.objects.list<T>(schema.kind) });
  const items = useMemo(() => {
    const all = sort ? [...(q.data?.items ?? [])].sort(sort) : (q.data?.items ?? []);
    const s = search.trim().toLowerCase();
    if (!s) return all;
    return all.filter((o) => o.metadata.name.toLowerCase().includes(s) || objectComment(o).toLowerCase().includes(s));
  }, [q.data, search, sort]);

  const invalidate = () => qc.invalidateQueries({ queryKey: qk.objects(schema.kind) });

  const save = useMutation({
    mutationFn: (obj: AnyObject) => api.objects.put(schema.kind, obj),
    onSuccess: (_r, obj) => {
      void invalidate();
      onSaved?.(obj);
    },
  });

  const toggle = useMutation({
    mutationFn: (v: { obj: T; enabled: boolean }) => api.objects.put(schema.kind, withMeta(v.obj, objectComment(v.obj), v.enabled)),
    onSuccess: (_r, v) => {
      notify.success(`${v.obj.metadata.name} ${v.enabled ? 'enabled' : 'disabled'}`);
      void invalidate();
    },
    onError: (err, v) => notify.error(err, `Could not change ${v.obj.metadata.name}`),
  });

  const batchDelete = useMutation({
    mutationFn: async (keys: string[]) => {
      // :batchDelete takes names in one namespace; group by namespace.
      const byNs = new Map<string, string[]>();
      for (const k of keys) {
        const [ns, name] = k.split('/') as [string, string];
        byNs.set(ns, [...(byNs.get(ns) ?? []), name]);
      }
      const results = await Promise.all([...byNs].map(([ns, names]) => api.objects.batchDelete(schema.kind, names, ns || undefined)));
      return {
        success: results.flatMap((r) => r.processed.success),
        errors: results.flatMap((r) => r.processed.errors),
      };
    },
    onSuccess: (r) => {
      if (r.success.length) notify.success(`Deleted ${r.success.length} ${r.success.length === 1 ? schema.singular : schema.plural}`);
      for (const e of r.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      setSelected(new Set());
      setConfirm(false);
      void invalidate();
    },
    onError: (err) => notify.error(err, 'Batch delete failed'),
  });

  const rows = useMemo(() => items.map((o): Row<T> => ({ id: key(o), obj: o })), [items]);
  const { selectionConfig } = useTableSelectionState({
    data: rows,
    idKey: 'id',
    selectedKeys: selected,
    setSelectedKeys: setSelected,
    getIsItemEnabled: (r) => objectOwnership(r.obj).editable,
  });
  const selection = useTableSelection({ ...selectionConfig, getRowLabel: (r: Row<T>) => r.obj.metadata.name });

  // Proportional widths only: a column's minWidth / weight sets the table's
  // minimum width, so the floors stay low enough to fit 1280 px with the sidebar.
  const tableColumns: TableColumn<Row<T>>[] = [
    {
      key: 'name',
      header: 'Name',
      width: proportional(1.2, { minWidth: 96 }),
      renderCell: ({ obj: o }) => {
        const comment = objectComment(o);
        return (
          <VStack gap={0.5} align="start">
            <Text>
              {detailPath ? (
                <Link href={detailPath(o)} weight="semibold" color="inherit">
                  {o.metadata.name}
                </Link>
              ) : (
                <Button label={o.metadata.name} size="sm" variant="ghost" onClick={() => setEditing(o)} />
              )}
              {o.metadata.namespace && o.metadata.namespace !== 'default' ? <Text type="supporting"> ({o.metadata.namespace})</Text> : null}
            </Text>
            {comment ? <Text type="supporting">{comment}</Text> : null}
          </VStack>
        );
      },
    },
    ...columns.map((c): TableColumn<Row<T>> => {
      const align = c.align ?? (c.className?.split(/\s+/).includes('num') ? 'end' : undefined);
      return {
        key: `col-${c.header}`,
        header: c.header,
        align,
        width: c.width ?? (align === 'end' ? proportional(0.75, { minWidth: 60 }) : proportional(1, { minWidth: 80 })),
        renderCell: (r) => c.cell(r.obj),
      };
    }),
    {
      key: 'managed',
      header: 'Managed by',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: ({ obj: o }) => <ManagedBadge owner={o.metadata.annotations?.[MANAGED_BY]} />,
    },
    {
      key: 'enabled',
      header: 'Enabled',
      width: proportional(0.9, { minWidth: 72 }),
      renderCell: ({ obj: o }) => (
        <Switch
          label={`${o.metadata.name} enabled`}
          isLabelHidden
          size="sm"
          value={objectEnabled(o)}
          isDisabled={!objectOwnership(o).editable || !canWrite || toggle.isPending}
          onChange={(on) => toggle.mutate({ obj: o, enabled: on })}
        />
      ),
    },
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      // Room for Open, and for a row action (Refresh) beside it.
      width: rowActions ? proportional(2, { minWidth: 160 }) : proportional(0.9, { minWidth: 72 }),
      align: 'end',
      renderCell: ({ obj: o }) => (
        <HStack gap={1} hAlign="end" vAlign="center">
          {rowActions?.(o)}
          {detailPath ? (
            <Button label="Open" size="sm" variant="ghost" href={detailPath(o)} />
          ) : (
            <Button label={objectOwnership(o).editable ? 'Edit' : 'View'} size="sm" variant="ghost" onClick={() => setEditing(o)} />
          )}
        </HStack>
      ),
    },
  ];

  return (
    <Page>
      <PageHeader
        title={title}
        description={description}
        actions={
          <>
            {headerActions}
            <Button
              label={`New ${schema.singular}`}
              variant="primary"
              isDisabled={!canWrite || !!createBlocked}
              tooltip={canWrite ? createBlocked : `Needs ${schema.resource}.write`}
              onClick={() => (CreateWizard ? setCreating(true) : setEditing('new'))}
            />
          </>
        }
      />
      {children}
      <HStack gap={2} vAlign="center" wrap="wrap">
        <TextField fieldWidth={400} label={`Search ${schema.plural}`} isLabelHidden startIcon="search" placeholder={`Search ${schema.plural} by name or comment`} value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
        <StackItem size="fill" />
        {selected.size > 0 ? (
          <Button label={`Delete ${selected.size} selected`} variant="destructive" isDisabled={!canDelete} onClick={() => setConfirm(true)} />
        ) : null}
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <Card padding={2}>
          <Table<Row<T>>
            aria-label={title}
            density="compact"
            hasHover
            idKey="id"
            data={rows}
            columns={tableColumns}
            plugins={{ selection, rowAttrs: rowAttrs as unknown as TablePlugin<Row<T>> }}
            emptyState={<Text type="supporting">{search ? `No ${schema.plural} match "${search}".` : `No ${schema.plural} yet.`}</Text>}
          />
        </Card>
      ) : null}

      {CreateWizard ? <CreateWizard isOpen={creating} onClose={() => setCreating(false)} /> : null}

      <FormDialog
        isOpen={editing !== null}
        onClose={() => setEditing(null)}
        title={editing === null ? '' : editing === 'new' ? `New ${schema.singular}` : `${objectOwnership(editing).editable ? 'Edit' : 'View'} ${schema.singular} ${editing.metadata.name}`}
        width={760}
      >
        {editing !== null ? (
          <ObjectForm
            schema={schema}
            initial={editing === 'new' ? undefined : editing}
            onCancel={() => setEditing(null)}
            onSubmit={async (obj) => {
              await save.mutateAsync(obj);
              notify.success(`${editing === 'new' ? 'Created' : 'Saved'} ${obj.metadata.name}`);
              setEditing(null);
            }}
          />
        ) : null}
      </FormDialog>

      <FormDialog
        isOpen={confirm}
        onClose={() => setConfirm(false)}
        title={`Delete ${selected.size} ${selected.size === 1 ? schema.singular : schema.plural}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(false)} />
            <Button label="Delete" variant="destructive" isLoading={batchDelete.isPending} onClick={() => batchDelete.mutate([...selected])} />
          </>
        }
      >
        <List density="compact" listStyle="disc">
          {[...selected].map((k) => (
            <ListItem key={k} label={k.replace(/^default\//, '').replace(/^\//, '')} />
          ))}
        </List>
        <Text type="supporting" as="p">
          Policies that reference them stop using them. This cannot be undone.
        </Text>
      </FormDialog>
    </Page>
  );
}
