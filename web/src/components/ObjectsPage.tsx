// The shared list page of Lists, Groups and Policies (§10 principle 6):
// search, enabled switches and comments on every row, "managed by" badges,
// multi-select with :batchDelete, and the generated create/edit form in a
// dialog. Objects managed elsewhere are listed but not selectable and open
// read-only.
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Dialog } from '@astryxdesign/core/Dialog';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useMemo, useState, type ReactNode } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { AnyObject } from '../api/types';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, withMeta } from '../lib/managedBy';
import type { KindSchema } from '../lib/objectSchemas';
import { useSearchParams } from '../lib/router';
import { ErrorBanner, Loading, useNotify } from './Feedback';
import { ManagedBadge } from './ManagedBadge';
import { ObjectForm } from './ObjectForm';

export interface Column<T> {
  header: string;
  cell: (obj: T) => ReactNode;
  className?: string;
}

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
}

function key(o: AnyObject): string {
  return `${o.metadata.namespace ?? ''}/${o.metadata.name}`;
}

export function ObjectsPage<T extends AnyObject>({ title, description, schema, columns, headerActions, rowActions, children, onSaved }: ObjectsPageProps<T>) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const [editing, setEditing] = useState<T | 'new' | null>(null);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [confirm, setConfirm] = useState(false);
  const titleId = useId();
  const confirmId = useId();
  const canWrite = can(schema.resource, 'write');
  const canDelete = can(schema.resource, 'delete');

  const q = useQuery({ queryKey: qk.objects(schema.kind), queryFn: () => api.objects.list<T>(schema.kind) });
  const items = useMemo(() => {
    const all = q.data?.items ?? [];
    const s = search.trim().toLowerCase();
    if (!s) return all;
    return all.filter((o) => o.metadata.name.toLowerCase().includes(s) || objectComment(o).toLowerCase().includes(s));
  }, [q.data, search]);

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

  const selectable = items.filter((o) => objectOwnership(o).editable);
  const allSelected = selectable.length > 0 && selectable.every((o) => selected.has(key(o)));

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>{title}</h1>
          <p className="k-muted k-small">{description}</p>
        </div>
        <div className="k-actions">
          {headerActions}
          <Button label={`New ${schema.singular}`} variant="primary" isDisabled={!canWrite} tooltip={canWrite ? undefined : `Needs ${schema.resource}.write`} onClick={() => setEditing('new')} />
        </div>
      </div>
      {children}
      <div className="k-row">
        <TextInput label={`Search ${schema.plural}`} isLabelHidden placeholder={`Search ${schema.plural} by name or comment`} value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
        <span className="k-spacer" />
        {selected.size > 0 ? (
          <Button label={`Delete ${selected.size} selected`} variant="destructive" isDisabled={!canDelete} onClick={() => setConfirm(true)} />
        ) : null}
      </div>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label={title}>
            <thead>
              <tr>
                <th scope="col" className="k-col-check">
                  <CheckboxInput
                    label={`Select all ${schema.plural}`}
                    isLabelHidden
                    value={allSelected ? true : selected.size > 0 ? 'indeterminate' : false}
                    isDisabled={selectable.length === 0}
                    onChange={(on) => setSelected(on ? new Set(selectable.map(key)) : new Set())}
                  />
                </th>
                <th scope="col">Name</th>
                <th scope="col">Enabled</th>
                {columns.map((c) => (
                  <th key={c.header} scope="col" className={c.className}>
                    {c.header}
                  </th>
                ))}
                <th scope="col">Managed by</th>
                <th scope="col">
                  <span className="k-visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td colSpan={columns.length + 5} className="k-muted">
                    {search ? `No ${schema.plural} match "${search}".` : `No ${schema.plural} yet.`}
                  </td>
                </tr>
              ) : null}
              {items.map((o) => {
                const own = objectOwnership(o);
                const k = key(o);
                const comment = objectComment(o);
                return (
                  <tr key={k} data-testid={`row-${o.metadata.name}`}>
                    <td className="k-col-check">
                      <CheckboxInput
                        label={`Select ${o.metadata.name}`}
                        isLabelHidden
                        value={selected.has(k)}
                        isDisabled={!own.editable}
                        onChange={(on) =>
                          setSelected((prev) => {
                            const next = new Set(prev);
                            if (on) next.add(k);
                            else next.delete(k);
                            return next;
                          })
                        }
                      />
                    </td>
                    <td>
                      <button type="button" className="k-linklike" onClick={() => setEditing(o)}>
                        {o.metadata.name}
                      </button>
                      {o.metadata.namespace && o.metadata.namespace !== 'default' ? <span className="k-muted k-small"> ({o.metadata.namespace})</span> : null}
                      {comment ? <div className="k-muted k-small">{comment}</div> : null}
                    </td>
                    <td>
                      <Switch
                        label={`${o.metadata.name} enabled`}
                        isLabelHidden
                        size="sm"
                        value={objectEnabled(o)}
                        isDisabled={!own.editable || !canWrite || toggle.isPending}
                        onChange={(on) => toggle.mutate({ obj: o, enabled: on })}
                      />
                    </td>
                    {columns.map((c) => (
                      <td key={c.header} className={c.className}>
                        {c.cell(o)}
                      </td>
                    ))}
                    <td>
                      <ManagedBadge owner={o.metadata.annotations?.[MANAGED_BY]} />
                    </td>
                    <td className="k-nowrap">
                      {rowActions?.(o)}
                      <Button label={own.editable ? 'Edit' : 'View'} size="sm" variant="ghost" onClick={() => setEditing(o)} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      <Dialog isOpen={editing !== null} onOpenChange={(open) => !open && setEditing(null)} purpose="form" width={760} maxHeight="90dvh" aria-labelledby={titleId}>
        {editing !== null ? (
          <div className="k-stack-lg">
            <h2 id={titleId} className="k-dialog-title">
              {editing === 'new' ? `New ${schema.singular}` : `${objectOwnership(editing).editable ? 'Edit' : 'View'} ${schema.singular} ${editing.metadata.name}`}
            </h2>
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
          </div>
        ) : null}
      </Dialog>

      <Dialog isOpen={confirm} onOpenChange={setConfirm} purpose="required" width={460} aria-labelledby={confirmId}>
        <div className="k-stack-lg">
          <h2 id={confirmId} className="k-dialog-title">
            Delete {selected.size} {selected.size === 1 ? schema.singular : schema.plural}?
          </h2>
          <ul className="k-small">
            {[...selected].map((k) => (
              <li key={k} className="k-mono">
                {k.replace(/^default\//, '').replace(/^\//, '')}
              </li>
            ))}
          </ul>
          <p className="k-muted k-small">Policies that reference them stop using them. This cannot be undone.</p>
          <div className="k-row">
            <span className="k-spacer" />
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(false)} />
            <Button label="Delete" variant="destructive" isLoading={batchDelete.isPending} onClick={() => batchDelete.mutate([...selected])} />
          </div>
        </div>
      </Dialog>
    </div>
  );
}
