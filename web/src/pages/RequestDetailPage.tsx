// One unblock request (/requests/:ns/:name): the current decision in the
// header (state, when it was asked and decided, by whom, the note and the
// allow entry in force), the request as the block page recorded it, the
// recent queries of that client for the name from the query log of the
// node that took the request, and the decision history. Approve and Deny
// open the decision dialogs; Delete asks first.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ScopeParams } from '../api/endpoints';
import { useDeleteUnblock } from '../api/hooks';
import type { QueryFilter, UnblockAllow, UnblockDecision, UnblockRequest } from '../api/types';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { QueryLogTable } from '../components/QueryLogTable';
import { ApproveDialog, DenyDialog } from '../components/unblock/UnblockDialogs';
import { ClientText, RefLinks, StateBadge, When, WhyBlocked } from '../components/unblock/UnblockParts';
import { formatDateTime, formatExact } from '../lib/format';
import { objectHref, shortRef } from '../lib/policies';
import { Link, useNavigate } from '../lib/router';
import { canApprove, canDeny } from '../lib/unblock';
import type { PageProps } from '../routes';

const RECENT = 50;

export default function RequestDetailPage({ params }: PageProps) {
  const ns = params.ns ?? 'default';
  const name = params.name ?? '';
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const navigate = useNavigate();
  const [dialog, setDialog] = useState<'approve' | 'deny' | 'delete' | null>(null);
  const q = useQuery({ queryKey: qk.unblock('get', ns, name), queryFn: () => api.unblock.get(ns, name) });
  const del = useDeleteUnblock();
  const r = q.data;

  if (q.error) {
    return (
      <div className="k-page">
        <Link href="/requests" className="k-small">
          ← Requests
        </Link>
        <h1>Request {name}</h1>
        <ErrorBanner error={q.error} />
      </div>
    );
  }
  if (!r) return <Loading />;

  const canDecide = can('unblockrequests', 'decide');
  const decideTip = canDecide ? undefined : 'Needs unblockrequests.decide';
  const remove = () =>
    del.mutate(
      { namespace: ns, name },
      {
        onSuccess: () => {
          notify.success(`Deleted the request for ${r.domain}`);
          navigate('/requests');
        },
        onError: (err) => notify.error(err, 'Could not delete the request'),
      },
    );

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/requests" className="k-small">
            ← Requests
          </Link>
          <h1 className="k-break">{r.domain}</h1>
          <span className="k-row k-small">
            <StateBadge state={r.state} />
            <span>
              requested <When ts={r.requestedAt} />
              {r.submissions > 1 ? ` · asked ${formatExact(r.submissions)} times` : ''}
            </span>
            {r.decidedAt ? (
              <span>
                decided <When ts={r.decidedAt} />
                {r.decidedBy ? ` by ${r.decidedBy}` : ''}
              </span>
            ) : null}
            {r.state === 'Pending' && r.expiresAt ? (
              <span className="k-muted">
                expires undecided <When ts={r.expiresAt} />
              </span>
            ) : null}
          </span>
        </div>
        <div className="k-actions">
          {canApprove(r) ? <Button label="Approve" variant="primary" isDisabled={!canDecide} tooltip={decideTip} onClick={() => setDialog('approve')} /> : null}
          {canDeny(r) ? <Button label="Deny" variant="secondary" isDisabled={!canDecide} tooltip={decideTip} onClick={() => setDialog('deny')} /> : null}
          <Button
            label="Delete"
            variant="destructive"
            isDisabled={!can('unblockrequests', 'delete')}
            tooltip={can('unblockrequests', 'delete') ? undefined : 'Needs unblockrequests.delete'}
            onClick={() => setDialog('delete')}
          />
        </div>
      </div>

      <DecisionPanel r={r} />
      <RequestSection r={r} />
      <RecentQueries r={r} />
      <HistorySection r={r} />

      {dialog === 'approve' ? <ApproveDialog request={r} onClose={() => setDialog(null)} /> : null}
      {dialog === 'deny' ? <DenyDialog request={r} onClose={() => setDialog(null)} /> : null}
      <FormDialog
        isOpen={dialog === 'delete'}
        onClose={() => setDialog(null)}
        title={`Delete the request for ${r.domain}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setDialog(null)} />
            <Button label="Delete request" variant="destructive" isLoading={del.isPending} onClick={remove} />
          </>
        }
      >
        <p className="k-small">
          {r.state === 'Approved' && r.allow && !r.allow.expiresAt
            ? `The permanent allow entry ${r.allow.entry} stays on ${shortRef(r.allow.list)}; remove it on the Domains page if it is no longer needed.`
            : r.state === 'Approved' && r.allow?.expiresAt
              ? 'A time-limited approval in force cannot be deleted: deny it first, which removes its allow entry.'
              : 'The request and its history are removed. The user can ask again from the block page.'}
        </p>
      </FormDialog>
    </div>
  );
}

/** The allow entry of an approval: entry, list link, scope, policies, end. */
function AllowView({ allow }: { allow: UnblockAllow }) {
  return (
    <span className="k-stack-sm">
      <span>
        <span className="k-mono">{allow.entry}</span> on <Link href={objectHref('lists', allow.list)}>{shortRef(allow.list)}</Link>{' '}
        <Badge variant="info" label={allow.match === 'subdomains' ? '+ subdomains' : 'exact'} />
      </span>
      <span className="k-small">
        {allow.scope === 'group' && allow.group ? (
          <>
            for group <Link href={objectHref('groups', allow.group)}>{shortRef(allow.group)}</Link>
          </>
        ) : (
          'for everyone'
        )}
        {allow.policies.length ? (
          <>
            {' '}
            · attached to <RefLinks kind="policies" refs={allow.policies} empty="" />
          </>
        ) : null}
      </span>
      <span className="k-small">
        {allow.removed ? (
          <span className="k-muted">
            released <When ts={allow.removed} />
          </span>
        ) : allow.expiresAt ? (
          <span>
            until <time dateTime={allow.expiresAt}>{formatDateTime(allow.expiresAt)}</time> (<When ts={allow.expiresAt} />)
          </span>
        ) : (
          'permanent'
        )}
        {!allow.added ? <span className="k-muted"> · the entry was on the list already, so it stays when the approval ends</span> : null}
      </span>
    </span>
  );
}

function DecisionPanel({ r }: { r: UnblockRequest }) {
  return (
    <section className="k-panel" aria-labelledby="req-decision" data-testid="request-decision">
      <div className="k-panel-title">
        <h2 id="req-decision">Decision</h2>
      </div>
      {r.state === 'Pending' ? (
        <p className="k-muted">Not decided yet{r.expiresAt ? `; the request expires undecided on ${formatDateTime(r.expiresAt)}` : ''}.</p>
      ) : (
        <dl className="k-kv">
          <dt>State</dt>
          <dd>
            <StateBadge state={r.state} />
          </dd>
          <dt>Decided</dt>
          <dd>
            {r.decidedAt ? (
              <>
                <time dateTime={r.decidedAt}>{formatDateTime(r.decidedAt)}</time>
                {r.decidedBy ? ` by ${r.decidedBy}` : ''}
              </>
            ) : (
              <span className="k-muted">{r.state === 'Expired' ? 'expired undecided' : '–'}</span>
            )}
          </dd>
          <dt>Note</dt>
          <dd>{r.note ? <span className="k-reason">{r.note}</span> : <span className="k-muted">none</span>}</dd>
          {r.allow ? (
            <>
              <dt>Allow entry</dt>
              <dd>
                <AllowView allow={r.allow} />
              </dd>
            </>
          ) : null}
        </dl>
      )}
    </section>
  );
}

function RequestSection({ r }: { r: UnblockRequest }) {
  return (
    <section className="k-panel k-section" aria-labelledby="req-request" data-testid="request-details">
      <h2 id="req-request" className="k-h3">
        Request
      </h2>
      <dl className="k-kv">
        <dt>Name given</dt>
        <dd>{r.requesterName ? <span className="k-break">{r.requesterName}</span> : <span className="k-muted">none</span>}</dd>
        <dt>Contact</dt>
        <dd>{r.contact ? <span className="k-break">{r.contact}</span> : <span className="k-muted">none</span>}</dd>
        <dt>Client</dt>
        <dd>
          <ClientText r={r} />
        </dd>
        <dt>Groups</dt>
        <dd>
          <RefLinks kind="groups" refs={r.groups} empty="no client group" />
        </dd>
        <dt>Policies</dt>
        <dd>
          <RefLinks kind="policies" refs={r.policies} empty="none" />
        </dd>
        <dt>Node</dt>
        <dd>{r.node ?? <span className="k-muted">not recorded</span>}</dd>
        <dt>Reason</dt>
        <dd>{r.reason ? <span className="k-reason">{r.reason}</span> : <span className="k-muted">none given</span>}</dd>
        <dt>Why blocked</dt>
        <dd>
          <WhyBlocked block={r.block} detailed />
        </dd>
        <dt>First asked</dt>
        <dd>
          <time dateTime={r.createdAt}>{formatDateTime(r.createdAt)}</time>
        </dd>
      </dl>
    </section>
  );
}

/** The query-log filter for the requester's queries of the name. */
export function recentFilter(r: Pick<UnblockRequest, 'domain' | 'client' | 'clientPrivacy'>): QueryFilter {
  // Under prefix privacy the query log records the same network address.
  return { domain: r.domain, ...(r.client && r.clientPrivacy !== 'hidden' ? { client: r.client } : {}), limit: RECENT };
}

function RecentQueries({ r }: { r: UnblockRequest }) {
  const api = useApi();
  const can = useCan();
  const current = useScopeParams();
  // The query log is per node: read the one whose block page took the request.
  const scope: ScopeParams = r.node ? { scope: 'node', node: r.node } : current;
  const filter = recentFilter(r);
  const q = useQuery({ queryKey: qk.queries('unblock', scope, filter), queryFn: () => api.queries.list(scope, filter), enabled: can('querylog', 'read') });
  const all = new URLSearchParams({ domain: r.domain, ...(filter.client ? { client: filter.client } : {}) });
  return (
    <section className="k-stack" aria-labelledby="req-recent">
      <div className="k-panel-title">
        <h2 id="req-recent" className="k-h3">
          Recent queries
        </h2>
        <Link href={`/queries?${all.toString()}`}>Open in the query log</Link>
      </div>
      <p className="k-muted k-small">
        {filter.client ? 'Queries of this client for the name' : 'Queries of every client for the name (the client was not recorded)'}
        {r.node ? `, from the query log of ${r.node}, the node that took the request.` : '.'}
      </p>
      {!can('querylog', 'read') ? <Banner status="info" title="Needs querylog.read" description="Your role cannot read the query log." /> : null}
      {q.error ? <ErrorBanner error={q.error} title="The query log could not be read" /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && !q.data.queries.length ? <p className="k-muted">No queries in the log.</p> : null}
      {q.data?.queries.length ? <QueryLogTable rows={q.data.queries} hasMore={false} loadingMore={false} onLoadMore={() => undefined} canAct={false} /> : null}
    </section>
  );
}

function HistorySection({ r }: { r: UnblockRequest }) {
  const current: UnblockDecision | null =
    r.state === 'Pending'
      ? null
      : { state: r.state, ...(r.decidedBy ? { decidedBy: r.decidedBy } : {}), ...(r.decidedAt ? { decidedAt: r.decidedAt } : {}), ...(r.note ? { note: r.note } : {}), ...(r.allow ? { allow: r.allow } : {}) };
  const rows = [...(current ? [{ d: current, now: true }] : []), ...[...r.history].reverse().map((d) => ({ d, now: false }))];
  return (
    <section className="k-panel k-section" aria-labelledby="req-history">
      <h2 id="req-history" className="k-h3">
        Decision history
      </h2>
      {rows.length ? (
        <div className="k-table-wrap">
          <table className="k-table" aria-label="Decision history">
            <thead>
              <tr>
                <th scope="col">Decision</th>
                <th scope="col">When</th>
                <th scope="col">By</th>
                <th scope="col">Note</th>
                <th scope="col">Allow entry</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ d, now }, i) => (
                <tr key={i}>
                  <td className="k-nowrap">
                    <StateBadge state={d.state} /> {now ? <span className="k-muted k-small">current</span> : null}
                  </td>
                  <td className="k-small k-nowrap">{d.decidedAt ? <time dateTime={d.decidedAt}>{formatDateTime(d.decidedAt)}</time> : <span className="k-muted">–</span>}</td>
                  <td className="k-small">{d.decidedBy ?? <span className="k-muted">–</span>}</td>
                  <td className="k-small">{d.note ? <span className="k-reason">{d.note}</span> : <span className="k-muted">–</span>}</td>
                  <td className="k-small">{d.allow ? <AllowView allow={d.allow} /> : <span className="k-muted">–</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="k-muted">No decision yet.</p>
      )}
      {r.state === 'Pending' && r.history.length ? <p className="k-small">The user asked again after the last decision, so the request is pending once more.</p> : null}
    </section>
  );
}
