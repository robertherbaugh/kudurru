// Requests (/requests): what users asked for on the block page, newest
// first (GET /unblockrequests), filtered by state (Pending by default,
// kept in ?state=) and optionally by name (?domain=, matched here: the API
// filters by state only), paged with `continue`. Rows link to the request
// page; Approve and Deny open the decision dialogs for users with
// unblockrequests.decide.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { pixel, proportional, Table, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Token } from '@astryxdesign/core/Token';
import { VStack } from '@astryxdesign/core/VStack';
import { useInfiniteQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import { useFeature } from '../api/hooks';
import type { UnblockCounts, UnblockRequest, UnblockState } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { FeatureLocked } from '../components/FeatureLocked';
import { EnumSelect } from '../components/fields/EnumSelect';
import { Page, PageHeader } from '../components/layout/Page';
import { ApproveDialog, DenyDialog } from '../components/unblock/UnblockDialogs';
import { domainKey, Requester, StateBadge, Typed, When, WhyBlocked } from '../components/unblock/UnblockParts';
import { formatExact } from '../lib/format';
import { useSearchParams } from '../lib/router';
import { STATE_FILTER, UNBLOCK_STATES, canApprove, canDeny, requestHref } from '../lib/unblock';

const PAGE = 50;
/** With a name filter the page asks for the API's largest page and matches here. */
const DOMAIN_PAGE = 500;

function stateParam(v: string | null): UnblockState | 'all' {
  if (v === 'all') return 'all';
  return UNBLOCK_STATES.find((s) => s === v) ?? 'Pending';
}

function countOf(c: UnblockCounts | undefined, s: string): number | undefined {
  if (!c) return undefined;
  if (s === 'all') return c.pending + c.approved + c.denied + c.expired;
  return c[s.toLowerCase() as keyof UnblockCounts];
}

type Row = UnblockRequest & Record<string, unknown>;

/** Data attributes on each body row (tests and e2e find rows by them). */
const rowAttrs: TablePlugin<Row> = {
  transformBodyRow: (props, r) => ({
    ...props,
    htmlProps: { ...props.htmlProps, ...({ 'data-testid': `request-${r.domain}`, 'data-request': `${r.namespace}/${r.name}` } as Record<string, string>) },
  }),
};

/** Unblock requests are licensed (feature "requests"); locked, the page asks for nothing. */
export default function RequestsPage() {
  const f = useFeature('requests');
  if (!f.ready) {
    return (
      <Page>
        <PageHeader title="Requests" />
        <Loading />
      </Page>
    );
  }
  if (!f.allowed) {
    return (
      <Page>
        <PageHeader title="Requests" description="What users asked for on the block page." />
        <FeatureLocked feature="requests" what="Unblock requests" />
      </Page>
    );
  }
  return <RequestsView />;
}

function RequestsView() {
  const api = useApi();
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const state = stateParam(sp.get('state'));
  const domain = domainKey(sp.get('domain') ?? '');
  const [approving, setApproving] = useState<UnblockRequest | null>(null);
  const [denying, setDenying] = useState<UnblockRequest | null>(null);
  const canDecide = can('unblockrequests', 'decide');
  const limit = domain ? DOMAIN_PAGE : PAGE;

  const q = useInfiniteQuery({
    queryKey: qk.unblock('list', state, limit),
    queryFn: ({ pageParam }) => api.unblock.list({ state: state === 'all' ? undefined : state, limit, continue: pageParam || undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => last.continue || undefined,
    refetchInterval: 30_000,
  });
  const loaded = useMemo(() => q.data?.pages.flatMap((p) => p.items) ?? [], [q.data]);
  const rows = useMemo(() => (domain ? loaded.filter((r) => domainKey(r.domain) === domain) : loaded) as Row[], [loaded, domain]);
  const counts = q.data?.pages[0]?.counts;
  // The counts cover every name, so a name filter shows the states without them.
  const options = STATE_FILTER.map((o) => {
    const n = domain ? undefined : countOf(counts, o.value);
    return n === undefined ? o : { ...o, label: `${o.label} (${formatExact(n)})` };
  });

  const columns: TableColumn<Row>[] = [
    {
      key: 'domain',
      header: 'Domain',
      width: proportional(1.2, { minWidth: 120 }),
      renderCell: (r) => (
        <Link href={requestHref(r)} color="inherit" weight="semibold" hasUnderline>
          <Text type="code" color="inherit" maxLines={3} wordBreak="break-all">
            {r.domain}
          </Text>
        </Link>
      ),
    },
    { key: 'requester', header: 'Requester', width: proportional(1.6, { minWidth: 160 }), renderCell: (r) => <Requester r={r} /> },
    {
      key: 'requestedAt',
      header: 'Requested',
      width: pixel(110),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <Text type="supporting" color="primary" textWrap="nowrap">
            <When ts={r.requestedAt} />
          </Text>
          {r.submissions > 1 ? <Text type="supporting">asked {formatExact(r.submissions)} times</Text> : null}
        </VStack>
      ),
    },
    {
      key: 'reason',
      header: 'Reason',
      width: proportional(1.4, { minWidth: 140 }),
      renderCell: (r) =>
        r.reason ? (
          <Text type="supporting" color="primary">
            <Typed>{r.reason}</Typed>
          </Text>
        ) : (
          <Text type="supporting">none given</Text>
        ),
    },
    {
      key: 'block',
      header: 'Why blocked',
      width: proportional(1.2, { minWidth: 130 }),
      renderCell: (r) => (
        <Text type="supporting" color="primary">
          <WhyBlocked block={r.block} />
        </Text>
      ),
    },
    { key: 'state', header: 'State', width: pixel(96), renderCell: (r) => <StateBadge state={r.state} /> },
    {
      key: 'actions',
      header: 'Actions',
      width: pixel(110),
      renderCell: (r) => (
        <VStack gap={1} align="start">
          {canDecide && canApprove(r) ? <Button label="Approve" size="sm" variant="primary" onClick={() => setApproving(r)} /> : null}
          {canDecide && canDeny(r) ? <Button label="Deny" size="sm" variant="secondary" onClick={() => setDenying(r)} /> : null}
        </VStack>
      ),
    },
  ];

  const noun = state === 'all' ? 'requests' : `${state.toLowerCase()} requests`;

  return (
    <Page>
      <PageHeader
        title="Requests"
        description="Users ask on the block page for a blocked name to be allowed. Approving adds an allow entry (for everyone or for the requester's group); the user sees the decision and your note on the block page."
      />

      <HStack gap={2} vAlign="end" wrap="wrap">
        <EnumSelect label="State" value={state} options={options} onChange={(v) => setParams({ state: v === 'Pending' ? null : v })} />
        {domain ? <Token label={`Domain: ${domain}`} description="Only the requests for this name" onRemove={() => setParams({ domain: null })} data-testid="domain-filter" /> : null}
      </HStack>

      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && !rows.length ? (
        <Banner
          status="info"
          title={domain ? `No ${noun} for ${domain}${q.hasNextPage ? ` in the newest ${formatExact(loaded.length)}` : ''}` : state === 'all' ? 'No requests yet' : `No ${noun}`}
          description={
            !domain && state === 'Pending'
              ? 'Requests appear here when a user asks on the block page; the block page offers the form while unblockRequests.enabled and blockPage.enabled are on.'
              : undefined
          }
        />
      ) : null}
      {rows.length ? (
        <Card>
          <Table<Row> aria-label="Unblock requests" density="compact" verticalAlign="top" hasHover idKey={(r) => `${r.namespace}/${r.name}`} data={rows} columns={columns} plugins={{ rowAttrs }} />
        </Card>
      ) : null}
      {q.hasNextPage ? (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Text type="supporting">
            {domain ? `${formatExact(rows.length)} for ${domain} in the newest ${formatExact(loaded.length)} of ${formatExact(countOf(counts, state))}` : `${formatExact(rows.length)} of ${formatExact(countOf(counts, state))}`}
          </Text>
          <Button label="Load more" size="sm" variant="secondary" isLoading={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </HStack>
      ) : null}

      {approving ? <ApproveDialog key={`${approving.namespace}/${approving.name}`} request={approving} onClose={() => setApproving(null)} /> : null}
      {denying ? <DenyDialog key={`${denying.namespace}/${denying.name}`} request={denying} onClose={() => setDenying(null)} /> : null}
    </Page>
  );
}
