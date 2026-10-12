// One unblock request (/requests/:ns/:name): the current decision in the
// header (state, when it was asked and decided, by whom, the note and the
// allow entry in force), the request as the block page recorded it, the
// recent queries of that client for the name from the query log of the
// node that took the request, and the decision history. Approve and Deny
// open the decision dialogs; Delete asks first.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useQuery } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ScopeParams } from '../api/endpoints';
import { useDeleteUnblock } from '../api/hooks';
import type { QueryFilter, UnblockAllow, UnblockDecision, UnblockRequest } from '../api/types';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { QueryLogTable } from '../components/QueryLogTable';
import { ApproveDialog, DenyDialog } from '../components/unblock/UnblockDialogs';
import { ClientText, RefLinks, StateBadge, Typed, When, WhyBlocked } from '../components/unblock/UnblockParts';
import { formatDateTime, formatExact } from '../lib/format';
import { objectHref, shortRef } from '../lib/policies';
import { useNavigate } from '../lib/router';
import { canApprove, canDeny } from '../lib/unblock';
import type { PageProps } from '../routes';

const RECENT = 50;
const BACK = { href: '/requests', label: 'Requests' };

const muted = (text: string) => <Text type="inherit" color="secondary">{text}</Text>;

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
      <Page>
        <PageHeader title={`Request ${name}`} back={BACK} />
        <ErrorBanner error={q.error} />
      </Page>
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
    <Page>
      <PageHeader
        title={
          <Text type="inherit" maxLines={3} wordBreak="break-all">
            {r.domain}
          </Text>
        }
        back={BACK}
        meta={
          <>
            <StateBadge state={r.state} />
            <Text type="supporting" color="primary">
              requested <When ts={r.requestedAt} />
              {r.submissions > 1 ? ` · asked ${formatExact(r.submissions)} times` : ''}
            </Text>
            {r.decidedAt ? (
              <Text type="supporting" color="primary">
                decided <When ts={r.decidedAt} />
                {r.decidedBy ? ` by ${r.decidedBy}` : ''}
              </Text>
            ) : null}
            {r.state === 'Pending' && r.expiresAt ? (
              <Text type="supporting">
                expires undecided <When ts={r.expiresAt} />
              </Text>
            ) : null}
          </>
        }
        actions={
          <>
            {canApprove(r) ? <Button label="Approve" variant="primary" isDisabled={!canDecide} tooltip={decideTip} onClick={() => setDialog('approve')} /> : null}
            {canDeny(r) ? <Button label="Deny" variant="secondary" isDisabled={!canDecide} tooltip={decideTip} onClick={() => setDialog('deny')} /> : null}
            <Button
              label="Delete"
              variant="destructive"
              isDisabled={!can('unblockrequests', 'delete')}
              tooltip={can('unblockrequests', 'delete') ? undefined : 'Needs unblockrequests.delete'}
              onClick={() => setDialog('delete')}
            />
          </>
        }
      />

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
        <Text as="p">
          {r.state === 'Approved' && r.allow && !r.allow.expiresAt
            ? `The permanent allow entry ${r.allow.entry} stays on ${shortRef(r.allow.list)}; remove it on the Allowlist page if it is no longer needed.`
            : r.state === 'Approved' && r.allow?.expiresAt
              ? 'A time-limited approval in force cannot be deleted: deny it first, which removes its allow entry.'
              : 'The request and its history are removed. The user can ask again from the block page.'}
        </Text>
      </FormDialog>
    </Page>
  );
}

/** The allow entry of an approval: entry, list link, scope, policies, end. */
function AllowView({ allow }: { allow: UnblockAllow }) {
  return (
    <VStack gap={1.5}>
      <HStack gap={1} vAlign="center" wrap="wrap">
        <Text type="code">{allow.entry}</Text>
        <Text type="inherit">
          on{' '}
          <Link type="inherit" href={objectHref('lists', allow.list)} color="inherit" hasUnderline>
            {shortRef(allow.list)}
          </Link>
        </Text>
        <Badge variant="info" label={allow.match === 'subdomains' ? '+ subdomains' : 'exact'} />
      </HStack>
      <Text type="supporting" color="primary">
        {allow.scope === 'group' && allow.group ? (
          <>
            for group{' '}
            <Link type="inherit" href={objectHref('groups', allow.group)} color="inherit" hasUnderline>
              {shortRef(allow.group)}
            </Link>
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
      </Text>
      <Text type="supporting" color="primary">
        {allow.removed ? (
          <Text type="inherit" color="secondary">
            released <When ts={allow.removed} />
          </Text>
        ) : allow.expiresAt ? (
          <>
            until <time dateTime={allow.expiresAt}>{formatDateTime(allow.expiresAt)}</time> (<When ts={allow.expiresAt} />)
          </>
        ) : (
          'permanent'
        )}
        {!allow.added ? <Text type="inherit" color="secondary"> · the entry was on the list already, so it stays when the approval ends</Text> : null}
      </Text>
      {!allow.removed ? (
        <Text type="supporting" color="primary">
          <Link type="inherit" href={`/allowlist?q=${encodeURIComponent(allow.entry.replace(/^\|\||\^$/g, ''))}`} color="inherit" hasUnderline>
            Show it on the Allowlist
          </Link>
        </Text>
      ) : null}
    </VStack>
  );
}

function DecisionPanel({ r }: { r: UnblockRequest }) {
  return (
    <Panel title="Decision" headingId="req-decision" data-testid="request-decision">
      {r.state === 'Pending' ? (
        <Text color="secondary" as="p">
          Not decided yet{r.expiresAt ? `; the request expires undecided on ${formatDateTime(r.expiresAt)}` : ''}.
        </Text>
      ) : (
        <MetadataList label={{ position: 'start', width: 128 }}>
          <MetadataListItem label="State">
            <StateBadge state={r.state} />
          </MetadataListItem>
          <MetadataListItem label="Decided">
            {r.decidedAt ? (
              <Text>
                <time dateTime={r.decidedAt}>{formatDateTime(r.decidedAt)}</time>
                {r.decidedBy ? ` by ${r.decidedBy}` : ''}
              </Text>
            ) : (
              muted(r.state === 'Expired' ? 'expired undecided' : '–')
            )}
          </MetadataListItem>
          <MetadataListItem label="Note">{r.note ? <Typed>{r.note}</Typed> : muted('none')}</MetadataListItem>
          {r.allow ? (
            <MetadataListItem label="Allow entry">
              <AllowView allow={r.allow} />
            </MetadataListItem>
          ) : null}
        </MetadataList>
      )}
    </Panel>
  );
}

function RequestSection({ r }: { r: UnblockRequest }) {
  return (
    <Panel title="Request" headingId="req-request" data-testid="request-details">
      <MetadataList label={{ position: 'start', width: 128 }}>
        <MetadataListItem label="Name given">{r.requesterName ? <Typed>{r.requesterName}</Typed> : muted('none')}</MetadataListItem>
        <MetadataListItem label="Contact">{r.contact ? <Typed>{r.contact}</Typed> : muted('none')}</MetadataListItem>
        <MetadataListItem label="Client">
          <ClientText r={r} />
        </MetadataListItem>
        <MetadataListItem label="Groups">
          <RefLinks kind="groups" refs={r.groups} empty="no client group" />
        </MetadataListItem>
        <MetadataListItem label="Policies">
          <RefLinks kind="policies" refs={r.policies} empty="none" />
        </MetadataListItem>
        <MetadataListItem label="Node">{r.node ?? muted('not recorded')}</MetadataListItem>
        <MetadataListItem label="Reason">{r.reason ? <Typed>{r.reason}</Typed> : muted('none given')}</MetadataListItem>
        <MetadataListItem label="Why blocked">
          <WhyBlocked block={r.block} detailed />
        </MetadataListItem>
        <MetadataListItem label="First asked">
          <Text>
            <time dateTime={r.createdAt}>{formatDateTime(r.createdAt)}</time>
          </Text>
        </MetadataListItem>
      </MetadataList>
    </Panel>
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
  const headingId = useId();
  // The query log is per node: read the one whose block page took the request.
  const scope: ScopeParams = r.node ? { scope: 'node', node: r.node } : current;
  const filter = recentFilter(r);
  const q = useQuery({ queryKey: qk.queries('unblock', scope, filter), queryFn: () => api.queries.list(scope, filter), enabled: can('querylog', 'read') });
  const all = new URLSearchParams({ domain: r.domain, ...(filter.client ? { client: filter.client } : {}) });
  return (
    <VStack as="section" gap={2} aria-labelledby={headingId}>
      <HStack gap={2} hAlign="between" vAlign="center" wrap="wrap">
        <Heading level={3} accessibilityLevel={2} id={headingId}>
          Recent queries
        </Heading>
        <Link type="inherit" href={`/queries?${all.toString()}`} hasUnderline>
          Open in the query log
        </Link>
      </HStack>
      <Text type="supporting" as="p">
        {filter.client ? 'Queries of this client for the name' : 'Queries of every client for the name (the client was not recorded)'}
        {r.node ? `, from the query log of ${r.node}, the node that took the request.` : '.'}
      </Text>
      {!can('querylog', 'read') ? <Banner status="info" title="Needs querylog.read" description="Your role cannot read the query log." /> : null}
      {q.error ? <ErrorBanner error={q.error} title="The query log could not be read" /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && !q.data.queries.length ? (
        <Text color="secondary" as="p">
          No queries in the log.
        </Text>
      ) : null}
      {q.data?.queries.length ? <QueryLogTable rows={q.data.queries} hasMore={false} loadingMore={false} onLoadMore={() => undefined} canAct={false} /> : null}
    </VStack>
  );
}

interface HistoryRow extends Record<string, unknown> {
  d: UnblockDecision;
  now: boolean;
  i: number;
}

const HISTORY_COLUMNS: TableColumn<HistoryRow>[] = [
  {
    key: 'decision',
    header: 'Decision',
    width: pixel(150),
    renderCell: ({ d, now }) => (
      <HStack gap={1} vAlign="center">
        <StateBadge state={d.state} />
        {now ? <Text type="supporting">current</Text> : null}
      </HStack>
    ),
  },
  {
    key: 'when',
    header: 'When',
    width: proportional(1, { minWidth: 170 }),
    renderCell: ({ d }) =>
      d.decidedAt ? (
        <Text type="supporting" color="primary" textWrap="nowrap">
          <time dateTime={d.decidedAt}>{formatDateTime(d.decidedAt)}</time>
        </Text>
      ) : (
        muted('–')
      ),
  },
  { key: 'by', header: 'By', width: proportional(0.7, { minWidth: 80 }), renderCell: ({ d }) => (d.decidedBy ? <Text type="supporting" color="primary">{d.decidedBy}</Text> : muted('–')) },
  {
    key: 'note',
    header: 'Note',
    width: proportional(1.3, { minWidth: 120 }),
    renderCell: ({ d }) =>
      d.note ? (
        <Text type="supporting" color="primary">
          <Typed>{d.note}</Typed>
        </Text>
      ) : (
        muted('–')
      ),
  },
  { key: 'allow', header: 'Allow entry', width: proportional(2, { minWidth: 200 }), renderCell: ({ d }) => (d.allow ? <AllowView allow={d.allow} /> : muted('–')) },
];

function HistorySection({ r }: { r: UnblockRequest }) {
  const current: UnblockDecision | null =
    r.state === 'Pending'
      ? null
      : { state: r.state, ...(r.decidedBy ? { decidedBy: r.decidedBy } : {}), ...(r.decidedAt ? { decidedAt: r.decidedAt } : {}), ...(r.note ? { note: r.note } : {}), ...(r.allow ? { allow: r.allow } : {}) };
  const rows: HistoryRow[] = [...(current ? [{ d: current, now: true }] : []), ...[...r.history].reverse().map((d) => ({ d, now: false }))].map((x, i) => ({ ...x, i }));
  return (
    <Panel title="Decision history" headingId="req-history">
      {rows.length ? (
        <Table<HistoryRow> aria-label="Decision history" density="compact" idKey="i" data={rows} columns={HISTORY_COLUMNS} />
      ) : (
        <Text color="secondary" as="p">
          No decision yet.
        </Text>
      )}
      {r.state === 'Pending' && r.history.length ? (
        <Text type="supporting" color="primary" as="p">
          The user asked again after the last decision, so the request is pending once more.
        </Text>
      ) : null}
    </Panel>
  );
}
