// Block and allow lists: one row per list with its enable switch, mode,
// sources ("3 of 4 enabled"), entries, health and last update; rows open
// the list page (/lists/:ns/:name) and "New list" the wizard (?new=1). A
// block list with RPZ feeds also shows the worst feed state this node
// reports (GET /blocklists/_rpz, D391) under its health.
// Also the multi-add of exact or regex domains into the ui-allow / ui-deny
// lists and "which lists contain this domain" (GET /blocklists/_search).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { List, ListItem } from '@astryxdesign/core/List';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { qk, useApi, useCan, useCannotReason, usePrimaryOnly } from '../api/context';
import type { BlockList, ListSource, RPZFeed } from '../api/types';
import { ErrorBanner, errorText, useNotify } from '../components/Feedback';
import { ApiError } from '../api/client';
import { TextField } from '../components/fields/TextField';
import { AddEntryDialog } from '../components/domains/DomainEntryDialogs';
import { Panel } from '../components/layout/Page';
import { ObjectsPage } from '../components/ObjectsPage';
import { ListWizardDialog } from '../components/wizards/ListWizard';
import { formatExact, formatRelative } from '../lib/format';
import { listHealth } from '../lib/listHealth';
import { isRPZSource, listPath, sourceEnabled, sourceName, sourcesSummary } from '../lib/lists';
import { feedStateView, feedsOf, hasEnabledFeed, worstFeed } from '../lib/rpz';
import { RPZ_POLL_MS } from '../components/lists/RPZFeeds';
import { SCHEMAS } from '../lib/objectSchemas';

function ListSearch() {
  const api = useApi();
  const [domain, setDomain] = useState('');
  const [submitted, setSubmitted] = useState('');
  const q = useQuery({ queryKey: ['lists', 'search', submitted], queryFn: () => api.lists.search(submitted), enabled: !!submitted });
  return (
    <Panel title="Which lists contain a domain?" headingId="list-search">
      <HStack
        as="form"
        gap={2}
        vAlign="end"
        wrap="wrap"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(domain.trim());
        }}
      >
        <TextField label="Domain" startIcon="search" value={domain} onChange={setDomain} placeholder="ads.example.com" hasClear />
        <Button type="submit" label="Search" variant="secondary" />
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.data ? (
        q.data.items.length === 0 ? (
          <Text color="secondary" as="p">
            No list contains {submitted}.
          </Text>
        ) : (
          <List density="compact" aria-label={`Lists that contain ${submitted}`}>
            {q.data.items.map((h) => (
              <ListItem
                key={`${h.list}/${h.mode}/${h.rule}`}
                startContent={<Badge variant={h.mode === 'allow' ? 'success' : 'error'} label={h.mode} />}
                label={h.list}
                description={
                  <>
                    matches with <Code size="inherit">{h.rule}</Code>
                  </>
                }
              />
            ))}
          </List>
        )
      ) : null}
    </Panel>
  );
}

/** Whether every source is inline, so there is nothing to fetch and no "last fetched" (an RPZ feed is transferred, not inline). */
export function inlineOnly(sources: ListSource[] | undefined): boolean {
  return !!sources?.length && sources.every((s) => !s.url && !s.configMapRef && !isRPZSource(s));
}

/** How long the page polls list status after a create or refresh. */
export const STATUS_POLL_MS = 20_000;

/** The sources one by one, a line each, for the Sources cell's tooltip. */
function describeSources(sources: ListSource[]): string[] {
  return sources.map((s, i) => {
    const what = isRPZSource(s) ? `RPZ feed ${s.rpz?.zoneName ?? sourceName(i, s)}` : (s.url ?? (s.configMapRef ? `ConfigMap ${s.configMapRef.name}` : `${s.inline?.length ?? 0} inline entries`));
    return sourceEnabled(s) ? what : `${what} (off)`;
  });
}

/**
 * The worst RPZ feed state of a list on this node, or why it is unknown:
 * plain text while every feed serves, a badge only when clients get
 * SERVFAIL or a refresh failed (Astryx: badges for states that need action).
 */
function FeedCell({ list, feeds, unknown }: { list: BlockList; feeds: RPZFeed[] | undefined; unknown: string | undefined }) {
  if (unknown !== undefined || feeds === undefined) {
    return (
      <Tooltip content={unknown ?? "Reading this node's feed state"} hasHoverIndication={false}>
        <Text type="supporting" size="sm">
          RPZ unknown
        </Text>
      </Tooltip>
    );
  }
  const mine = feedsOf(feeds, list);
  const worst = worstFeed(mine);
  if (!worst) {
    return (
      <Tooltip content="This node reports no feed for this list yet; its configuration may not include the list." hasHoverIndication={false}>
        <Text type="supporting" size="sm">
          RPZ not reported
        </Text>
      </Tooltip>
    );
  }
  const view = feedStateView(worst);
  const detail = [`${worst.source ?? worst.origin ?? 'feed'}${mine.length > 1 ? ` (worst of ${mine.length} feeds)` : ''} on ${worst.node ? `node ${worst.node}` : 'this node'}`, worst.message].filter(Boolean).join(': ');
  const tip = view.servfail ? `${detail} Clients whose policy uses this list get SERVFAIL from this node.` : detail;
  return (
    <Tooltip content={tip} hasHoverIndication={false}>
      {view.tone === 'success' ? (
        <Text size="sm" data-testid="rpz-state">
          RPZ {view.label}
        </Text>
      ) : (
        <Badge variant={view.tone} label={`RPZ ${view.label}`} data-testid="rpz-state" />
      )}
    </Tooltip>
  );
}

export default function ListsPage() {
  const api = useApi();
  const can = useCan();
  const cannot = useCannotReason();
  const primaryOnly = usePrimaryOnly();
  const addDomainsTip = can('blocklists', 'write') ? primaryOnly('createAllowlistEntries') : 'Needs blocklists.write';
  const notify = useNotify();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  // The leader compiles a new or refreshed list in the background; poll its
  // status for a short while so Entries and Updated fill in without a reload.
  const [polling, setPolling] = useState(0);
  const pollStatus = () => setPolling((n) => n + 1);
  useEffect(() => {
    if (!polling) return undefined;
    const t = window.setTimeout(() => setPolling(0), STATUS_POLL_MS);
    return () => window.clearTimeout(t);
  }, [polling]);
  useQuery({
    queryKey: qk.objects('blocklists'),
    queryFn: () => api.objects.list<BlockList>('blocklists'),
    enabled: polling > 0,
    refetchInterval: polling > 0 ? 2_000 : false,
  });
  // This node's feed state, read only while a block list has an enabled feed.
  const lists = useQuery({ queryKey: qk.objects('blocklists'), queryFn: () => api.objects.list<BlockList>('blocklists'), enabled: false });
  const anyFeed = (lists.data?.items ?? []).some((l) => l.spec.mode !== 'allow' && hasEnabledFeed(l));
  const feeds = useQuery({ queryKey: ['rpz', 'all'], queryFn: () => api.lists.rpzFeeds(), enabled: anyFeed && can('blocklists', 'read'), refetchInterval: RPZ_POLL_MS, retry: false });
  const feedsUnknown = feeds.error ? (feeds.error instanceof ApiError && feeds.error.status === 501 ? 'This node runs no RPZ feed manager' : `Could not read this node's feed state: ${errorText(feeds.error)}`) : undefined;
  const refresh = useMutation({
    mutationFn: (l: BlockList) => api.lists.refresh(l.metadata.namespace ?? 'default', l.metadata.name),
    onSuccess: (_r, l) => {
      notify.success(`Refreshing ${l.metadata.name}`);
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
      pollStatus();
    },
    onError: (err, l) => notify.error(err, `Could not refresh ${l.metadata.name}`),
  });
  return (
    <ObjectsPage<BlockList>
      title="Lists"
      description="Block and allow lists from URLs, inline entries or ConfigMaps. Policies decide which client groups use them."
      schema={SCHEMAS.blocklists}
      onSaved={pollStatus}
      detailPath={listPath}
      wizard={ListWizardDialog}
      headerActions={<Button label="Add domains" variant="secondary" isDisabled={!!addDomainsTip} tooltip={addDomainsTip ?? 'Add to the Allowlist or Denylist'} onClick={() => setAdding(true)} />}
      rowActions={(l) =>
        l.spec.sources.some((s) => s.url && sourceEnabled(s)) ? (
          <Button label="Refresh" size="sm" variant="ghost" isDisabled={!can('blocklists', 'refresh')} tooltip={cannot('blocklists', 'refresh')} isLoading={refresh.isPending && refresh.variables === l} onClick={() => refresh.mutate(l)} />
        ) : null
      }
      columns={[
        { header: 'Mode', cell: (l) => <Badge variant={l.spec.mode === 'allow' ? 'success' : 'error'} label={l.spec.mode ?? 'block'} /> },
        {
          header: 'Sources',
          // Room for "3 of 4 enabled" on one line.
          width: proportional(1.5, { minWidth: 120 }),
          cell: (l) => (
            <Tooltip
              content={
                <VStack gap={0}>
                  {describeSources(l.spec.sources).map((line, i) => (
                    <Text key={i} type="inherit" color="inherit">
                      {line}
                    </Text>
                  ))}
                </VStack>
              }
              hasHoverIndication={false}
            >
              <Text size="sm" textWrap="nowrap">
                {sourcesSummary(l.spec.sources)}
              </Text>
            </Tooltip>
          ),
        },
        { header: 'Entries', align: 'end', width: pixel(88), cell: (l) => formatExact(l.status?.entries) },
        {
          header: 'Health',
          // Room for the longer states ("unavailable, cached", "OK, 12 invalid").
          width: proportional(1.9, { minWidth: 152 }),
          cell: (l) => {
            const h = listHealth(l);
            const health = (
              <Tooltip content={h.detail} isEnabled={!!h.detail} hasHoverIndication={false}>
                <Badge variant={h.tone === 'neutral' ? 'neutral' : h.tone} label={h.label} />
              </Tooltip>
            );
            if (l.spec.mode === 'allow' || !hasEnabledFeed(l)) return health;
            return (
              <VStack gap={0.5} hAlign="start">
                {health}
                <FeedCell list={l} feeds={feeds.data ? (feeds.data.items ?? []) : undefined} unknown={feedsUnknown} />
              </VStack>
            );
          },
        },
        {
          header: 'Updated',
          cell: (l) =>
            l.status?.lastFetched ? (
              <time dateTime={l.status.lastFetched} title={l.status.lastFetched}>
                {formatRelative(l.status.lastFetched)}
              </time>
            ) : inlineOnly(l.spec.sources) ? (
              <Tooltip content="Inline entries are compiled from the object; there is nothing to fetch" hasHoverIndication={false}>
                <Text color="secondary">inline</Text>
              </Tooltip>
            ) : (
              '–'
            ),
        },
      ]}
    >
      <ListSearch />
      <AddEntryDialog key={adding ? 'open' : 'closed'} open={adding} onClose={() => setAdding(false)} />
    </ObjectsPage>
  );
}
