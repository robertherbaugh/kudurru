// Domains (like Pi-hole's Domains page): every exact and regex allow/deny
// entry the "Add domains" flow and the query-log actions wrote, one row
// each, read from the inline sources of the ui-allow and ui-deny lists
// (POST /domains/{allow|deny}/{exact|regex}, internal/api/lists.go). Entries
// are removed through the inline entries API (PATCH …/entries, ux-plan C2).
// The server keeps no per-entry switch or comment: an entry is on its list
// or not, and the whole list has the enable switch.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { BlockList } from '../api/types';
import { EnumSelect } from '../components/fields/EnumSelect';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { AddDomainsDialog, DOMAIN_TYPES, type DomainAction, type DomainKind, type DomainType } from '../components/lists/AddDomainsDialog';
import { formatExact } from '../lib/format';
import { listNamespace, listPath, sourceKind, sourceName } from '../lib/lists';
import { objectComment, objectEnabled, objectOwnership, withMeta } from '../lib/managedBy';
import { Link, useSearchParams } from '../lib/router';

/** The lists the Add domains flow writes (internal/api UIAllowList / UIDenyList). */
export const UI_LISTS = ['ui-deny', 'ui-allow'];

export interface DomainRow {
  key: string;
  value: string;
  action: DomainAction;
  kind: DomainKind;
  wildcard: boolean;
  list: BlockList;
  source: string;
  editable: boolean;
}

const PAGE = 100;

function entriesLabel(n: number): string {
  return `${formatExact(n)} ${n === 1 ? 'entry' : 'entries'}`;
}

/** One row per inline entry of the UI lists. */
export function domainRows(lists: BlockList[]): DomainRow[] {
  return lists
    .filter((l) => UI_LISTS.includes(l.metadata.name))
    .flatMap((l) => {
      const action: DomainAction = l.spec.mode === 'allow' ? 'allow' : 'deny';
      const editable = objectOwnership(l).editable;
      return l.spec.sources.flatMap((src, i) =>
        sourceKind(src) !== 'inline'
          ? []
          : (src.inline ?? []).map((value) => ({
              key: `${listNamespace(l)}/${l.metadata.name}/${sourceName(i, src)}/${value}`,
              value,
              action,
              kind: (src.format === 'regex' || (value.startsWith('/') && value.endsWith('/') && value.length > 2) ? 'regex' : 'exact'),
              wildcard: value.startsWith('*.') || value.startsWith('||'),
              list: l,
              source: sourceName(i, src),
              editable,
            })),
      );
    });
}

export default function DomainsPage() {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [sp, setParams] = useSearchParams();
  const search = sp.get('q') ?? '';
  const type = (sp.get('type') ?? '') as DomainType | '';
  const [page, setPage] = useState(0);
  const [adding, setAdding] = useState(false);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [confirm, setConfirm] = useState<DomainRow[] | null>(null);
  const canWrite = can('blocklists', 'write');

  const q = useQuery({ queryKey: qk.objects('blocklists'), queryFn: () => api.objects.list<BlockList>('blocklists') });
  const uiLists = (q.data?.items ?? []).filter((l) => UI_LISTS.includes(l.metadata.name));
  const all = useMemo(() => domainRows(q.data?.items ?? []), [q.data]);
  const rows = useMemo(() => {
    const s = search.trim().toLowerCase();
    return all.filter((r) => (!type || `${r.action}:${r.kind}` === type) && (!s || r.value.toLowerCase().includes(s)));
  }, [all, search, type]);
  const shown = rows.slice(page * PAGE, (page + 1) * PAGE);
  const selectable = shown.filter((r) => r.editable && canWrite);
  const allSelected = selectable.length > 0 && selectable.every((r) => selected.has(r.key));

  const remove = useMutation({
    mutationFn: async (items: DomainRow[]) => {
      // One PATCH per list source.
      const groups = new Map<string, DomainRow[]>();
      for (const r of items) {
        const k = `${listNamespace(r.list)}\u0000${r.list.metadata.name}\u0000${r.source}`;
        groups.set(k, [...(groups.get(k) ?? []), r]);
      }
      for (const group of groups.values()) {
        const first = group[0];
        if (!first) continue;
        await api.lists.patchEntries(listNamespace(first.list), first.list.metadata.name, first.source, { remove: group.map((r) => r.value) });
      }
      return items.length;
    },
    onSuccess: (n) => {
      notify.success(`Removed ${n} ${n === 1 ? 'domain' : 'domains'}`);
      setSelected(new Set());
      setConfirm(null);
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err) => notify.error(err, 'Could not remove'),
  });

  const toggleList = useMutation({
    mutationFn: (v: { list: BlockList; enabled: boolean }) => api.objects.put('blocklists', withMeta(v.list, objectComment(v.list), v.enabled)),
    onSuccess: (_r, v) => {
      notify.success(`${v.list.metadata.name} ${v.enabled ? 'enabled' : 'disabled'}`);
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err, v) => notify.error(err, `Could not change ${v.list.metadata.name}`),
  });

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Domains</h1>
          <p className="k-muted k-small">
            Single domains and expressions you allowed or denied, from Add domains and the query log. They live in the ui-deny and ui-allow lists; big lists are on the Lists page.
          </p>
        </div>
        <div className="k-actions">
          <Button label="Add domains" variant="primary" isDisabled={!canWrite} tooltip={canWrite ? undefined : 'Needs blocklists.write'} onClick={() => setAdding(true)} />
        </div>
      </div>

      {uiLists.length ? (
        <div className="k-row" aria-label="UI lists">
          {uiLists.map((l) => {
            const own = objectOwnership(l);
            return (
              <div key={`${listNamespace(l)}/${l.metadata.name}`} className="k-panel k-ui-list k-small" data-testid={`ui-list-${l.metadata.name}`}>
                <Switch
                  label={`${l.metadata.name} enabled`}
                  isLabelHidden
                  size="sm"
                  value={objectEnabled(l)}
                  isDisabled={!own.editable || !canWrite || toggleList.isPending}
                  onChange={(on) => toggleList.mutate({ list: l, enabled: on })}
                />
                <Link href={listPath(l)} className="k-mono">
                  {l.metadata.name}
                </Link>
                <Badge variant={l.spec.mode === 'allow' ? 'success' : 'error'} label={l.spec.mode === 'allow' ? 'allow' : 'deny'} />
                <span className="k-muted">{entriesLabel(domainRows([l]).length)}</span>
                {!objectEnabled(l) ? <Badge variant="warning" label="list disabled" /> : null}
              </div>
            );
          })}
        </div>
      ) : null}
      <p className="k-muted k-small">Entries have no switch of their own: remove one to stop it, or turn its whole list off above.</p>

      <div className="k-row-end">
        <TextInput
          className="k-search"
          label="Search domains"
          isLabelHidden
          placeholder="Search domains and expressions"
          value={search}
          onChange={(v) => {
            setParams({ q: v || null });
            setPage(0);
          }}
          hasClear
        />
        <EnumSelect
          label="Type"
          value={type}
          options={DOMAIN_TYPES}
          emptyLabel="All types"
          onChange={(v) => {
            setParams({ type: v || null });
            setPage(0);
          }}
        />
        <span className="k-spacer" />
        {selected.size > 0 ? <Button label={`Remove ${selected.size} selected`} variant="destructive" onClick={() => setConfirm(all.filter((r) => selected.has(r.key)))} /> : null}
      </div>

      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && !uiLists.length ? <Banner status="info" title="No domains yet" description="Add domains here or from the query log; kudurru creates the ui-deny and ui-allow lists on first use." /> : null}
      {q.data && uiLists.length ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Domains">
            <thead>
              <tr>
                <th scope="col" className="k-col-check">
                  <CheckboxInput
                    label="Select every domain on this page"
                    isLabelHidden
                    value={allSelected ? true : selectable.some((r) => selected.has(r.key)) ? 'indeterminate' : false}
                    isDisabled={!selectable.length}
                    onChange={(on) =>
                      setSelected((prev) => {
                        const next = new Set(prev);
                        for (const r of selectable) {
                          if (on) next.add(r.key);
                          else next.delete(r.key);
                        }
                        return next;
                      })
                    }
                  />
                </th>
                <th scope="col">Domain</th>
                <th scope="col">Type</th>
                <th scope="col">List</th>
                <th scope="col">
                  <span className="k-visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {shown.length === 0 ? (
                <tr>
                  <td colSpan={5} className="k-muted">
                    {search || type ? 'No domains match.' : 'No domains yet.'}
                  </td>
                </tr>
              ) : null}
              {shown.map((r) => {
                const listOn = objectEnabled(r.list);
                return (
                  <tr key={r.key} data-testid={`domain-${r.value}`} className={listOn ? undefined : 'k-row-off'}>
                    <td className="k-col-check">
                      <CheckboxInput
                        label={`Select ${r.value}`}
                        isLabelHidden
                        value={selected.has(r.key)}
                        isDisabled={!r.editable || !canWrite}
                        onChange={(on) =>
                          setSelected((prev) => {
                            const next = new Set(prev);
                            if (on) next.add(r.key);
                            else next.delete(r.key);
                            return next;
                          })
                        }
                      />
                    </td>
                    <td className="k-mono k-break">{r.value}</td>
                    <td className="k-nowrap">
                      <Badge variant={r.action === 'allow' ? 'success' : 'error'} label={r.action === 'allow' ? 'allow' : 'deny'} /> <Badge variant={r.kind === 'regex' ? 'purple' : 'neutral'} label={r.kind} />
                      {r.wildcard ? (
                        <>
                          {' '}
                          <Badge variant="info" label="+ subdomains" />
                        </>
                      ) : null}
                    </td>
                    <td className="k-small">
                      <Link href={listPath(r.list)}>{r.list.metadata.name}</Link>
                      {!listOn ? <span className="k-muted"> (disabled)</span> : null}
                    </td>
                    <td className="k-row-actions">
                      {r.editable && canWrite ? <Button label="Remove" size="sm" variant="ghost" onClick={() => setConfirm([r])} /> : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}
      {rows.length > PAGE ? (
        <div className="k-row">
          <span className="k-muted k-small">
            {formatExact(page * PAGE + 1)}–{formatExact(Math.min(rows.length, (page + 1) * PAGE))} of {formatExact(rows.length)}
          </span>
          <span className="k-spacer" />
          <Button label="Previous page" size="sm" variant="secondary" isDisabled={page === 0} onClick={() => setPage((p) => p - 1)} />
          <Button label="Next page" size="sm" variant="secondary" isDisabled={(page + 1) * PAGE >= rows.length} onClick={() => setPage((p) => p + 1)} />
        </div>
      ) : null}

      <AddDomainsDialog key={type || 'all'} open={adding} onClose={() => setAdding(false)} defaultType={type || 'deny:exact'} />

      <FormDialog
        isOpen={confirm !== null}
        onClose={() => setConfirm(null)}
        title={confirm?.length === 1 ? `Remove ${confirm[0]?.value ?? ''}?` : `Remove ${confirm?.length ?? 0} domains?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(null)} />
            <Button label="Remove" variant="destructive" isLoading={remove.isPending} onClick={() => confirm && remove.mutate(confirm)} />
          </>
        }
      >
        <p className="k-small">They leave their list at the next compile; add them again to undo.</p>
      </FormDialog>
    </div>
  );
}
