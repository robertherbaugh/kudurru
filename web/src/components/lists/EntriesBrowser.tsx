// The entries of one inline source, a page at a time (GET
// /blocklists/{ns}/{name}/sources/{source}/entries, ux-plan C2): search,
// paging, add one or many, remove the selected ones (PATCH …/entries
// {add, remove}). Replaces the text box that held every entry of the list.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { qk, useApi } from '../../api/context';
import type { BlockList, ListEntriesPatch } from '../../api/types';
import { formatExact } from '../../lib/format';
import { entriesProblem, listKey, listNamespace, parseEntries, sourceName } from '../../lib/lists';
import { EnumSelect } from '../fields/EnumSelect';
import { ErrorBanner, Loading, useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';

const PAGE_SIZES = [
  { value: '25', label: '25 per page' },
  { value: '50', label: '50 per page' },
  { value: '100', label: '100 per page' },
];

export interface EntriesBrowserProps {
  list: BlockList;
  index: number;
  editable: boolean;
  onClose: () => void;
}

export function EntriesBrowser({ list, index, editable, onClose }: EntriesBrowserProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const ns = listNamespace(list);
  const name = list.metadata.name;
  const src = list.spec.sources[index];
  const source = src ? sourceName(index, src) : '';
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const [pageSize, setPageSize] = useState(25);
  // cursors[i] is the continue token of page i ("" for the first page).
  const [cursors, setCursors] = useState<string[]>(['']);
  const page = cursors.length - 1;
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [adding, setAdding] = useState('');
  const [confirm, setConfirm] = useState(false);
  const [rejected, setRejected] = useState<{ item: string; error: string }[]>([]);

  // Debounced search; only a changed query goes back to the first page (the
  // mount-time run used to reset a page turned within the first 250 ms).
  useEffect(() => {
    const next = search.trim();
    if (next === query) return;
    const t = window.setTimeout(() => {
      setQuery(next);
      setCursors(['']);
    }, 250);
    return () => window.clearTimeout(t);
  }, [search, query]);

  const cursor = cursors[page] ?? '';
  const q = useQuery({
    queryKey: [...listKey(ns, name), 'entries', source, query, pageSize, cursor],
    queryFn: () => api.lists.entries(ns, name, source, { ...(query ? { q: query } : {}), limit: pageSize, ...(cursor ? { continue: cursor } : {}) }),
    placeholderData: keepPreviousData,
    enabled: !!source,
  });

  const patch = useMutation({
    mutationFn: (body: ListEntriesPatch) => api.lists.patchEntries(ns, name, source, body),
    onSuccess: (r, body) => {
      const errors = r?.processed?.errors ?? [];
      const okCount = r?.processed ? r.processed.success.length : (body.add?.length ?? 0) + (body.remove?.length ?? 0);
      setRejected(errors);
      if (body.add) {
        if (okCount) notify.success(`Added ${formatExact(okCount)} ${okCount === 1 ? 'entry' : 'entries'} to ${source}`);
        if (!errors.length) setAdding('');
      } else {
        notify.success(`Removed ${formatExact(okCount)} ${okCount === 1 ? 'entry' : 'entries'} from ${source}`);
        setSelected(new Set());
        setConfirm(false);
      }
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err, body) => notify.error(err, body.add ? 'Could not add the entries' : 'Could not remove the entries'),
  });

  const toAdd = parseEntries(adding);
  const addProblem = toAdd.length ? entriesProblem(toAdd, src?.format) : null;
  const items = q.data?.items ?? [];
  const pageValues = items.map((e) => e.value);
  const allOnPage = pageValues.length > 0 && pageValues.every((v) => selected.has(v));
  const total = src?.inline?.length ?? 0;
  const first = page * pageSize + 1;

  const select = (v: string, on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(v);
      else next.delete(v);
      return next;
    });

  return (
    <section className="k-panel k-stack-lg" aria-labelledby="entries-title" data-testid="entries-browser">
      <div className="k-panel-title">
        <div className="k-stack-sm">
          <h2 id="entries-title" className="k-h3">
            Entries of <span className="k-mono">{source}</span>
          </h2>
          <span className="k-muted k-small">
            {formatExact(total)} inline {total === 1 ? 'entry' : 'entries'}
            {src?.format && src.format !== 'auto' ? `, ${src.format} format` : ''}
          </span>
        </div>
        <Button label="Close entries" variant="ghost" onClick={onClose} />
      </div>

      {editable ? (
        <form
          className="k-row-end"
          onSubmit={(e) => {
            e.preventDefault();
            if (toAdd.length && !addProblem) patch.mutate({ add: toAdd });
          }}
        >
          <div className="k-grow">
            <TextArea
              label="Add entries"
              description="One or many, one per line."
              placeholder="ads.example.com"
              rows={2}
              hasSpellCheck={false}
              value={adding}
              onChange={(v) => {
                setAdding(v);
                setRejected([]);
              }}
              status={addProblem ? { type: 'error', message: `"${addProblem.first}": ${addProblem.error}` } : undefined}
            />
          </div>
          <Button type="submit" label={toAdd.length > 1 ? `Add ${toAdd.length}` : 'Add'} variant="secondary" isDisabled={!toAdd.length || !!addProblem} isLoading={patch.isPending && !!patch.variables?.add} />
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

      <div className="k-row-end">
        <TextInput className="k-search" label="Search entries" placeholder="Part of a domain or rule" value={search} onChange={setSearch} hasClear />
        <EnumSelect
          label="Page size"
          value={String(pageSize)}
          options={PAGE_SIZES}
          onChange={(v) => {
            setPageSize(Number(v));
            setCursors(['']);
          }}
        />
        <span className="k-spacer" />
        {editable && selected.size > 0 ? <Button label={`Remove ${selected.size} selected`} variant="destructive" onClick={() => setConfirm(true)} /> : null}
      </div>

      {q.error ? <ErrorBanner error={q.error} title="Could not read the entries" /> : null}
      {q.isLoading ? <Loading label="Loading entries" /> : null}
      {q.data ? (
        <div className="k-table-wrap">
          <table className="k-table" aria-label={`Entries of ${source}`}>
            <thead>
              <tr>
                {editable ? (
                  <th scope="col" className="k-col-check">
                    <CheckboxInput
                      label="Select every entry on this page"
                      isLabelHidden
                      value={allOnPage ? true : pageValues.some((v) => selected.has(v)) ? 'indeterminate' : false}
                      isDisabled={!pageValues.length}
                      onChange={(on) =>
                        setSelected((prev) => {
                          const next = new Set(prev);
                          for (const v of pageValues) {
                            if (on) next.add(v);
                            else next.delete(v);
                          }
                          return next;
                        })
                      }
                    />
                  </th>
                ) : null}
                <th scope="col" className="num k-col-line">
                  Line
                </th>
                <th scope="col">Entry</th>
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td colSpan={editable ? 3 : 2} className="k-muted">
                    {query ? `No entry contains "${query}".` : 'No entries.'}
                  </td>
                </tr>
              ) : null}
              {items.map((e) => (
                <tr key={`${e.line}-${e.value}`}>
                  {editable ? (
                    <td className="k-col-check">
                      <CheckboxInput label={`Select ${e.value}`} isLabelHidden value={selected.has(e.value)} onChange={(on) => select(e.value, on)} />
                    </td>
                  ) : null}
                  <td className="num k-muted">{e.line}</td>
                  <td className="k-mono k-break">{e.value}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <div className="k-row">
        <span className="k-muted k-small" role="status">
          {items.length ? `Showing ${formatExact(first)}–${formatExact(first + items.length - 1)}${query ? ` of the entries containing "${query}"` : ` of ${formatExact(total)}`}` : ''}
        </span>
        <span className="k-spacer" />
        <Button label="Previous page" size="sm" variant="secondary" isDisabled={page === 0} onClick={() => setCursors((c) => c.slice(0, -1))} />
        <Button label="Next page" size="sm" variant="secondary" isDisabled={!q.data?.continue || q.isPlaceholderData} onClick={() => setCursors((c) => (q.data?.continue ? [...c, q.data.continue] : c))} />
      </div>

      <FormDialog
        isOpen={confirm}
        onClose={() => setConfirm(false)}
        title={`Remove ${selected.size} ${selected.size === 1 ? 'entry' : 'entries'}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(false)} />
            <Button label="Remove" variant="destructive" isLoading={patch.isPending} onClick={() => patch.mutate({ remove: [...selected] })} />
          </>
        }
      >
        <ul className="k-small k-mono">
          {[...selected].slice(0, 20).map((v) => (
            <li key={v}>{v}</li>
          ))}
          {selected.size > 20 ? <li className="k-muted">and {selected.size - 20} more</li> : null}
        </ul>
        <p className="k-muted k-small">They are removed from {source}; add them again to undo.</p>
      </FormDialog>
    </section>
  );
}
