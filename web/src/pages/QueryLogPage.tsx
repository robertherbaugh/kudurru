// The query log (§10 principle 5): filters live in the URL (every drill-
// down is a shareable link), keyset pagination over `cursor`, wildcard
// search with suggestions, the live tail over SSE, and per-row Allow/Deny.
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useId, useMemo, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { QueryFilter, QueryRow, StatusClass } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { PurgeQueryLog } from '../components/PurgeQueryLog';
import { QueryLogTable } from '../components/QueryLogTable';
import { ScopeNote } from '../components/ScopeSelector';
import { SuggestInput } from '../components/SuggestInput';
import { formatDateTime, formatExact } from '../lib/format';
import { useSearchParams } from '../lib/router';
import { useLiveTail } from '../lib/useLiveTail';

/** The filter keys the page reads from and writes to the URL. */
export const FILTER_KEYS = ['client', 'domain', 'status', 'status_class', 'qtype', 'upstream', 'list', 'zone', 'since', 'until'] as const;

export const PAGE_SIZE = 100;
const LIVE_MAX = 2000;

const QTYPES = ['A', 'AAAA', 'HTTPS', 'CNAME', 'MX', 'TXT', 'SRV', 'PTR', 'NS', 'SOA', 'SVCB', 'CAA', 'ANY'];
const CLASSES: { value: StatusClass | ''; label: string }[] = [
  { value: '', label: 'Any status' },
  { value: 'permitted', label: 'Permitted' },
  { value: 'cached', label: 'Cached' },
  { value: 'blocked', label: 'Blocked' },
  { value: 'error', label: 'Errors' },
];

/** Reads the filter object out of the search params. */
export function filtersFrom(sp: URLSearchParams): QueryFilter {
  const f: Record<string, string> = {};
  for (const k of FILTER_KEYS) {
    const v = sp.get(k);
    if (v) f[k] = v;
  }
  return f;
}

/** ISO → the value of a datetime-local input (local time, minutes). */
export function toLocalInput(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A datetime-local value → ISO (UTC); "" stays "". */
export function fromLocalInput(v: string): string {
  if (!v) return '';
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

export default function QueryLogPage() {
  const api = useApi();
  const scope = useScopeParams();
  const can = useCan();
  const notify = useNotify();
  const qc = useQueryClient();
  const [sp, setParams] = useSearchParams();
  const filters = useMemo(() => filtersFrom(sp), [sp]);
  const filterKey = JSON.stringify(filters);
  const [live, setLive] = useState(false);
  const [liveRows, setLiveRows] = useState<QueryRow[]>([]);
  const [fresh, setFresh] = useState<ReadonlySet<number>>(new Set());
  const ids = useId();

  const q = useInfiniteQuery({
    queryKey: qk.queries('log', scope, filters),
    queryFn: ({ pageParam, signal }) => api.queries.list(scope, { ...filters, limit: PAGE_SIZE, ...(pageParam ? { cursor: pageParam } : {}) }, signal),
    initialPageParam: '',
    getNextPageParam: (last) => last.cursor_next || undefined,
    staleTime: 30_000,
  });

  // New filters or scope: the live buffer belongs to the old query.
  useEffect(() => {
    setLiveRows([]);
    setFresh(new Set());
  }, [filterKey, scope]);

  const onRows = useCallback((batch: QueryRow[]) => {
    setLiveRows((prev) => [...batch.slice().reverse(), ...prev].slice(0, LIVE_MAX));
    setFresh(new Set(batch.map((r) => r.id)));
  }, []);
  const tail = useLiveTail({ url: api.queries.streamURL(scope, filters), enabled: live, onRows });

  const rows = useMemo(() => {
    const seen = new Set<number>();
    const out: QueryRow[] = [];
    for (const r of [...liveRows, ...(q.data?.pages.flatMap((p) => p.queries) ?? [])]) {
      if (seen.has(r.id)) continue;
      seen.add(r.id);
      out.push(r);
    }
    return out;
  }, [liveRows, q.data]);

  const first = q.data?.pages[0];
  const action = useMutation({
    mutationFn: (v: { row: QueryRow; action: 'allow' | 'deny' }) => api.queries.action(v.row.id, v.action),
    onSuccess: (res, v) => {
      const ok = res.processed.success.length;
      const errs = res.processed.errors;
      if (ok) notify.success(`${v.action === 'allow' ? 'Allowed' : 'Denied'} ${v.row.domain} (list ui-${v.action})`);
      for (const e of errs) notify.error(new Error(`${e.item}: ${e.error}`));
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err, v) => notify.error(err, `Could not ${v.action} ${v.row.domain}`),
  });

  const set = (patch: Record<string, string | null>) => setParams(patch, { push: true });
  const loadMore = useCallback(() => {
    if (q.hasNextPage && !q.isFetchingNextPage) void q.fetchNextPage();
  }, [q]);

  const anyFilter = FILTER_KEYS.some((k) => sp.get(k));

  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Query log</h1>
        <div className="k-actions">
          <ScopeNote scope={first} />
          <Switch
            label="Live tail"
            description={tail === 'unsupported' ? 'This browser has no EventSource' : tail === 'retrying' ? 'Reconnecting…' : tail === 'live' ? 'Streaming new queries' : undefined}
            value={live}
            isDisabled={tail === 'unsupported'}
            onChange={setLive}
          />
          <PurgeQueryLog
            onPurged={() => {
              setLiveRows([]);
              void qc.invalidateQueries({ queryKey: qk.queries() });
            }}
          />
        </div>
      </div>

      <form
        className="k-filters"
        aria-label="Filters"
        onSubmit={(e) => {
          e.preventDefault();
        }}
      >
        <SuggestInput label="Domain" field="domain" value={filters.domain ?? ''} placeholder="*.example.com" onCommit={(v) => set({ domain: v || null })} />
        <SuggestInput label="Client" field="client" value={filters.client ?? ''} placeholder="IP or name" onCommit={(v) => set({ client: v || null })} />
        <label className="k-label" htmlFor={`${ids}-class`}>
          Status
          <select id={`${ids}-class`} className="k-native-select" value={filters.status_class ?? ''} onChange={(e) => set({ status_class: e.target.value || null })}>
            {CLASSES.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        </label>
        <label className="k-label" htmlFor={`${ids}-qtype`}>
          Type
          <select id={`${ids}-qtype`} className="k-native-select" value={filters.qtype ?? ''} onChange={(e) => set({ qtype: e.target.value || null })}>
            <option value="">Any type</option>
            {QTYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <SuggestInput label="Upstream" field="upstream" value={filters.upstream ?? ''} onCommit={(v) => set({ upstream: v || null })} />
        <SuggestInput label="List" field="list" value={filters.list ?? ''} onCommit={(v) => set({ list: v || null })} />
        <label className="k-label" htmlFor={`${ids}-since`}>
          Since
          <input
            id={`${ids}-since`}
            className="k-native-input"
            type="datetime-local"
            value={toLocalInput(filters.since)}
            onChange={(e) => set({ since: fromLocalInput(e.target.value) || null })}
          />
        </label>
        <label className="k-label" htmlFor={`${ids}-until`}>
          Until
          <input
            id={`${ids}-until`}
            className="k-native-input"
            type="datetime-local"
            value={toLocalInput(filters.until)}
            onChange={(e) => set({ until: fromLocalInput(e.target.value) || null })}
          />
        </label>
        {anyFilter ? (
          <Button
            label="Clear filters"
            variant="ghost"
            onClick={() => set(Object.fromEntries(FILTER_KEYS.map((k) => [k, null])))}
          />
        ) : null}
      </form>

      {filters.status || filters.zone ? (
        <p className="k-small">
          Also filtered by {filters.status ? <code>status={filters.status}</code> : null} {filters.zone ? <code>zone={filters.zone}</code> : null}
        </p>
      ) : null}

      {first ? (
        <p className="k-small k-muted" data-testid="log-totals">
          {formatExact(first.records_filtered)} of {formatExact(first.records_total)} queries match
          {first.earliest_ts_memory ? `; in memory since ${formatDateTime(first.earliest_ts_memory)}` : ''}
          {first.earliest_ts_disk ? `; on disk since ${formatDateTime(first.earliest_ts_disk)}` : ''}
        </p>
      ) : null}

      {q.error ? <ErrorBanner error={q.error} title="The query log could not be read" /> : null}

      <QueryLogTable
        rows={rows}
        fresh={fresh}
        hasMore={!!q.hasNextPage}
        loadingMore={q.isFetchingNextPage}
        onLoadMore={loadMore}
        canAct={can('blocklists', 'write')}
        onAction={(row, a) => action.mutate({ row, action: a })}
      />
    </div>
  );
}
