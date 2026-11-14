// The RPZ feeds of one block list as the answering node reports them (D391,
// GET /blocklists/{namespace}/{name}/rpz): per feed its state on this node
// (pending, serving, expired, unavailable), what that means for DNS, the
// reason and message the node's diagnosis names, serial, primary,
// confirmation, expiry, next attempt, last error, the compiled rules by
// trigger and action and every ignored reason with its count and examples.
// Feed state is node-local: another node can report something else. Only
// the fields the node sends are shown; nothing is derived for a missing one.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Code } from '@astryxdesign/core/Code';
import { Heading } from '@astryxdesign/core/Heading';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, pixel, Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { ApiError } from '../../api/client';
import { useApi } from '../../api/context';
import type { RPZFeed, RPZFeedList, RPZIgnoredReason } from '../../api/types';
import { formatDateTime, formatExact, formatRelative } from '../../lib/format';
import { feedStateView, ignoredReasonText, servfailText, worstFeed } from '../../lib/rpz';
import { ErrorBanner } from '../Feedback';
import { Panel } from '../layout/Page';

/** How often a read whose feeds all serve is repeated (like the secondary runtime). */
export const RPZ_POLL_MS = 30_000;
/**
 * How often a read is repeated while it is settling: the node's configuration
 * lacks the list yet (404) or has none (503), or a feed is not serving. A new
 * or changed feed then shows its first transfer within seconds instead of
 * after the next 30 s read.
 */
export const RPZ_SETTLING_POLL_MS = 3_000;

/** The prefix of every RPZ feed read (one list's and the Lists page's), for invalidation after a list change. */
export const RPZ_KEY = ['rpz'] as const;

export function rpzFeedsKey(namespace: string, name: string) {
  return [...RPZ_KEY, namespace, name] as const;
}

/** Whether a feed read is settling: 404 or 503, or a feed that is not serving. */
export function rpzSettling(data: RPZFeedList | undefined, error: unknown): boolean {
  if (error instanceof ApiError && (error.status === 404 || error.status === 503)) return true;
  if (error) return false;
  return (data?.items ?? []).some((f) => f.state !== 'serving');
}

/** The refetchInterval of a feed read: fast while it settles, else every 30 s. */
export function rpzPollInterval(query: { state: { data: RPZFeedList | undefined; error: unknown } }): number {
  return rpzSettling(query.state.data, query.state.error) ? RPZ_SETTLING_POLL_MS : RPZ_POLL_MS;
}

/** This node's feeds of one list (shared by the feed section and the list header). */
export function useRPZFeeds(namespace: string, name: string, enabled = true) {
  const api = useApi();
  return useQuery({ queryKey: rpzFeedsKey(namespace, name), queryFn: () => api.lists.rpz(namespace, name), refetchInterval: rpzPollInterval, retry: false, enabled });
}

/** The worst feed of a list on this node when clients get SERVFAIL from it, else undefined. */
export function servfailFeed(feeds: RPZFeed[] | undefined): RPZFeed | undefined {
  const worst = worstFeed(feeds ?? []);
  return worst && feedStateView(worst).servfail ? worst : undefined;
}

/** A timestamp with its relative form; '–' when the node sent none. */
function When({ at }: { at: string | undefined }) {
  if (!at) return <Text color="secondary">–</Text>;
  return (
    <time dateTime={at} title={formatDateTime(at)}>
      {formatDateTime(at)} ({formatRelative(at)})
    </time>
  );
}

function count(n: number | undefined): string {
  return n === undefined ? '–' : formatExact(n);
}

/** Why the node gave no feed list: a node without a feed manager, a list its configuration lacks, no configuration yet. */
function readProblem(error: unknown): { title: string; description: string } | null {
  if (!(error instanceof ApiError)) return null;
  if (error.status === 501) return { title: 'This node reports no RPZ feed state', description: 'The answering node runs no RPZ feed manager, so its feed state is unknown here. Ask the node with kudurru lists rpz.' };
  if (error.status === 404) return { title: "This node's configuration does not include the list yet", description: 'A new or changed list reaches the node with its next configuration snapshot; the feed state appears then.' };
  if (error.status === 503) return { title: 'This node has no configuration yet', description: 'It is starting or has not loaded its first configuration snapshot. The feed state appears once it has.' };
  return null;
}

export function RPZFeedsPanel({ namespace, name, listEnabled }: { namespace: string; name: string; listEnabled: boolean }) {
  const q = useRPZFeeds(namespace, name);
  const feeds = q.data?.items ?? [];
  const node = feeds.find((f) => f.node)?.node;
  const problem = readProblem(q.error);

  return (
    <Panel
      title="RPZ feeds on this node"
      headingId="list-rpz"
      data-testid="rpz-feeds"
      description={`Every node transfers its own copy of each feed, so this is ${node ? `node ${node}` : 'the answering node'}'s state; another node can differ. Queried again every 30 seconds, every few seconds while a feed is not serving.`}
    >
      {q.isPending ? (
        <Text type="supporting">Reading this node's feed state…</Text>
      ) : problem ? (
        <Banner status="info" title={problem.title} description={problem.description} />
      ) : q.error ? (
        <ErrorBanner error={q.error} title="Could not read this node's feed state" />
      ) : feeds.length === 0 ? (
        <Text type="supporting">
          {listEnabled ? 'This node reports no enabled RPZ feed for this list.' : 'The list is disabled, so no node transfers its feeds.'}
        </Text>
      ) : (
        <VStack gap={6}>
          {feeds.map((f, i) => (
            <FeedSection key={`${f.source ?? ''}/${f.origin ?? ''}/${i}`} feed={f} />
          ))}
        </VStack>
      )}
    </Panel>
  );
}

/** One feed: state, consequence and next step, the copy, the rules and what was ignored. */
export function FeedSection({ feed: f }: { feed: RPZFeed }) {
  const view = feedStateView(f);
  const ignored = f.ignored ?? [];
  const r = f.rules;
  const c = f.counts;
  const label = f.source ?? f.origin ?? 'feed';
  return (
    <VStack gap={3} data-testid={`rpz-feed-${label}`} data-state={f.state ?? ''}>
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Heading level={3}>
          <Code size="inherit">{label}</Code>
        </Heading>
        {f.origin && f.origin !== label ? <Text type="code" color="secondary">{f.origin}</Text> : null}
        <Badge variant={view.tone} label={view.label} />
      </HStack>

      {view.servfail ? (
        <Banner
          status={f.state === 'pending' ? 'warning' : 'error'}
          title={f.state === 'pending' ? 'Pending: clients get SERVFAIL' : `${view.label[0]?.toUpperCase() ?? ''}${view.label.slice(1)}: clients get SERVFAIL`}
          description={
            <VStack gap={1}>
              <Text type="inherit" color="inherit">
                {servfailText(f)}
              </Text>
              {f.message ? (
                <Text type="inherit" color="inherit" wordBreak="break-word" data-testid="rpz-message">
                  {f.reason ? `${f.reason}: ` : ''}
                  {f.message}
                </Text>
              ) : null}
              {f.nextAttemptAt ? (
                <Text type="inherit" color="inherit">
                  Next transfer attempt {formatRelative(f.nextAttemptAt)} ({formatDateTime(f.nextAttemptAt)}).
                </Text>
              ) : null}
            </VStack>
          }
        />
      ) : f.message ? (
        <Banner
          status="warning"
          title={f.reason === 'RPZTransfer' ? 'The last refresh failed' : (f.reason ?? 'Feed notice')}
          description={
            <Text type="inherit" color="inherit" wordBreak="break-word" data-testid="rpz-message">
              {f.message}
            </Text>
          }
        />
      ) : null}

      <MetadataList columns="multi">
        <MetadataListItem label="Node">{f.node ?? <Text color="secondary">–</Text>}</MetadataListItem>
        <MetadataListItem label={f.state === 'serving' ? 'Serial' : 'Last serial'}>{f.serial === undefined ? <Text color="secondary">no copy</Text> : String(f.serial)}</MetadataListItem>
        <MetadataListItem label="Primary">{f.primary ? <Text type="code">{f.primary}</Text> : <Text color="secondary">–</Text>}</MetadataListItem>
        <MetadataListItem label="Last confirmed">
          <When at={f.lastConfirmedAt} />
        </MetadataListItem>
        <MetadataListItem label={f.state === 'expired' ? 'Expired' : 'Expires'}>
          <When at={f.expiresAt} />
        </MetadataListItem>
        <MetadataListItem label="Next attempt">
          <When at={f.nextAttemptAt} />
        </MetadataListItem>
        <MetadataListItem label="Rules">{c ? `${count(c.rules)} of ${count(c.records)} records` : <Text color="secondary">no copy</Text>}</MetadataListItem>
        <MetadataListItem label="Ignored owners">{c ? `${count(c.unsupported)} unsupported, ${count(c.invalid)} invalid` : <Text color="secondary">–</Text>}</MetadataListItem>
        {c?.excluded || c?.staleExcludes ? (
          <MetadataListItem label="Excluded">
            {count(c.excluded)} switched off{c.staleExcludes ? `, ${count(c.staleExcludes)} exclusions match nothing` : ''}
          </MetadataListItem>
        ) : null}
      </MetadataList>

      {f.lastError && view.servfail ? (
        <MetadataList>
          <MetadataListItem label="Last error">
            <Text type="code" size="sm" wordBreak="break-all" data-testid="rpz-last-error">
              {f.lastError}
            </Text>
          </MetadataListItem>
        </MetadataList>
      ) : null}
      {f.configurationError && f.configurationError !== f.message ? (
        <MetadataList>
          <MetadataListItem label="Configuration error">
            <Text wordBreak="break-word">{f.configurationError}</Text>
          </MetadataListItem>
        </MetadataList>
      ) : null}

      {r ? (
        <MetadataList title="Compiled rules" columns="multi" data-testid="rpz-rules">
          <MetadataListItem label="QNAME">{count(r.qname)}</MetadataListItem>
          <MetadataListItem label="CLIENT-IP">{count(r.clientIP)}</MetadataListItem>
          <MetadataListItem label="RESPONSE-IP">{count(r.responseIP)}</MetadataListItem>
          <MetadataListItem label="NXDOMAIN">{count(r.nxdomain)}</MetadataListItem>
          <MetadataListItem label="NODATA">{count(r.nodata)}</MetadataListItem>
          <MetadataListItem label="PASSTHRU">{count(r.passthru)}</MetadataListItem>
          <MetadataListItem label="DROP">{count(r.drop)}</MetadataListItem>
          <MetadataListItem label="TCP-only">{count(r.tcpOnly)}</MetadataListItem>
          <MetadataListItem label="Local data">{count(r.localData)}</MetadataListItem>
        </MetadataList>
      ) : null}

      {ignored.length ? <IgnoredTable source={label} items={ignored} /> : null}
    </VStack>
  );
}

type IgnoredRow = { key: string; reason: string; unsupported: boolean | undefined; count: number | undefined; examples: string[] };

function IgnoredTable({ source, items }: { source: string; items: RPZIgnoredReason[] }) {
  const rows: IgnoredRow[] = items.map((x, i) => ({ key: `${x.reason ?? ''}-${i}`, reason: x.reason ?? '', unsupported: x.unsupported, count: x.count, examples: x.examples ?? [] }));
  return (
    <VStack gap={1}>
      <Text type="supporting">Ignored policy owners. The other rules of the feed still apply; docs/deploy/rpz.md lists what is supported.</Text>
      <Table<IgnoredRow>
        aria-label={`Ignored owners of ${source}`}
        density="compact"
        idKey="key"
        data={rows}
        columns={[
          {
            key: 'reason',
            header: 'Reason',
            width: proportional(2, { minWidth: 180 }),
            renderCell: (x) => (
              <VStack gap={0}>
                <Text>{ignoredReasonText(x.reason)}</Text>
                <Text type="code" size="sm" color="secondary">
                  {x.reason}
                </Text>
              </VStack>
            ),
          },
          {
            key: 'unsupported',
            header: 'Kind',
            width: pixel(120),
            renderCell: (x) => <Text size="sm">{x.unsupported === undefined ? '–' : x.unsupported ? 'unsupported' : 'malformed'}</Text>,
          },
          { key: 'count', header: 'Owners', align: 'end', width: pixel(88), renderCell: (x) => <Text>{count(x.count)}</Text> },
          {
            key: 'examples',
            header: 'Examples',
            width: proportional(3, { minWidth: 180 }),
            renderCell: (x) =>
              x.examples.length ? (
                <VStack gap={0}>
                  {x.examples.map((e) => (
                    <Text key={e} type="code" size="sm" wordBreak="break-all">
                      {e}
                    </Text>
                  ))}
                </VStack>
              ) : (
                <Text type="supporting">–</Text>
              ),
          },
        ]}
      />
    </VStack>
  );
}
