// Requests (/requests): what users asked for on the block page, newest
// first (GET /unblockrequests), filtered by state (Pending by default,
// kept in ?state=), paged with `continue`. Rows link to the request page;
// Approve and Deny open the decision dialogs for users with
// unblockrequests.decide.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { UnblockCounts, UnblockRequest, UnblockState } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { EnumSelect } from '../components/fields/EnumSelect';
import { ApproveDialog, DenyDialog } from '../components/unblock/UnblockDialogs';
import { Requester, StateBadge, When, WhyBlocked } from '../components/unblock/UnblockParts';
import { formatExact } from '../lib/format';
import { Link, useSearchParams } from '../lib/router';
import { STATE_FILTER, UNBLOCK_STATES, canApprove, canDeny, requestHref } from '../lib/unblock';

const PAGE = 50;

function stateParam(v: string | null): UnblockState | 'all' {
  if (v === 'all') return 'all';
  return UNBLOCK_STATES.find((s) => s === v) ?? 'Pending';
}

function countOf(c: UnblockCounts | undefined, s: string): number | undefined {
  if (!c) return undefined;
  if (s === 'all') return c.pending + c.approved + c.denied + c.expired;
  return c[s.toLowerCase() as keyof UnblockCounts];
}

export default function RequestsPage() {
  const api = useApi();
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const state = stateParam(sp.get('state'));
  const [approving, setApproving] = useState<UnblockRequest | null>(null);
  const [denying, setDenying] = useState<UnblockRequest | null>(null);
  const canDecide = can('unblockrequests', 'decide');

  const q = useInfiniteQuery({
    queryKey: qk.unblock('list', state),
    queryFn: ({ pageParam }) => api.unblock.list({ state: state === 'all' ? undefined : state, limit: PAGE, continue: pageParam || undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => last.continue || undefined,
    refetchInterval: 30_000,
  });
  const rows = useMemo(() => q.data?.pages.flatMap((p) => p.items) ?? [], [q.data]);
  const counts = q.data?.pages[0]?.counts;
  const options = STATE_FILTER.map((o) => {
    const n = countOf(counts, o.value);
    return n === undefined ? o : { ...o, label: `${o.label} (${formatExact(n)})` };
  });

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Requests</h1>
          <p className="k-muted k-small">
            Users ask on the block page for a blocked name to be allowed. Approving adds an allow entry (for everyone or for the requester's group); the user sees the decision and your
            note on the block page.
          </p>
        </div>
      </div>

      <div className="k-row-end">
        <EnumSelect label="State" value={state} options={options} onChange={(v) => setParams({ state: v === 'Pending' ? null : v })} />
      </div>

      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && !rows.length ? (
        <Banner
          status="info"
          title={state === 'all' ? 'No requests yet' : `No ${state.toLowerCase()} requests`}
          description={state === 'Pending' ? 'Requests appear here when a user asks on the block page; the block page offers the form while unblockRequests.enabled and blockPage.enabled are on.' : undefined}
        />
      ) : null}
      {rows.length ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table k-requests-table" aria-label="Unblock requests">
            <thead>
              <tr>
                <th scope="col">Domain</th>
                <th scope="col">Requester</th>
                <th scope="col">Requested</th>
                <th scope="col">Reason</th>
                <th scope="col">Why blocked</th>
                <th scope="col">State</th>
                <th scope="col">
                  <span className="k-visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.namespace}/${r.name}`} data-testid={`request-${r.domain}`} data-request={`${r.namespace}/${r.name}`}>
                  <td className="k-break">
                    <Link href={requestHref(r)} className="k-row-link k-mono">
                      {r.domain}
                    </Link>
                  </td>
                  <td>
                    <Requester r={r} />
                  </td>
                  <td className="k-nowrap k-small">
                    <When ts={r.requestedAt} />
                    {r.submissions > 1 ? <div className="k-muted">asked {formatExact(r.submissions)} times</div> : null}
                  </td>
                  <td className="k-small">{r.reason ? <span className="k-reason">{r.reason}</span> : <span className="k-muted">none given</span>}</td>
                  <td className="k-small">
                    <WhyBlocked block={r.block} />
                  </td>
                  <td>
                    <StateBadge state={r.state} />
                  </td>
                  <td className="k-row-actions">
                    <span className="k-row">
                      {canDecide && canApprove(r) ? <Button label="Approve" size="sm" variant="primary" onClick={() => setApproving(r)} /> : null}
                      {canDecide && canDeny(r) ? <Button label="Deny" size="sm" variant="secondary" onClick={() => setDenying(r)} /> : null}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {q.hasNextPage ? (
        <div className="k-row">
          <span className="k-muted k-small">
            {formatExact(rows.length)} of {formatExact(countOf(counts, state))}
          </span>
          <Button label="Load more" size="sm" variant="secondary" isLoading={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </div>
      ) : null}

      {approving ? <ApproveDialog key={`${approving.namespace}/${approving.name}`} request={approving} onClose={() => setApproving(null)} /> : null}
      {denying ? <DenyDialog key={`${denying.namespace}/${denying.name}`} request={denying} onClose={() => setDenying(null)} /> : null}
    </div>
  );
}
