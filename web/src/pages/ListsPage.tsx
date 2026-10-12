// Block and allow lists: one row per list with its enable switch, mode,
// sources ("3 of 4 enabled"), entries, health and last update; rows open
// the list page (/lists/:ns/:name) and "New list" the wizard (?new=1).
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
import { qk, useApi, useCan } from '../api/context';
import type { BlockList, ListSource } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { TextField } from '../components/fields/TextField';
import { AddEntryDialog } from '../components/domains/DomainEntryDialogs';
import { Panel } from '../components/layout/Page';
import { ObjectsPage } from '../components/ObjectsPage';
import { ListWizardDialog } from '../components/wizards/ListWizard';
import { formatExact, formatRelative } from '../lib/format';
import { listHealth } from '../lib/listHealth';
import { listPath, sourceEnabled, sourcesSummary } from '../lib/lists';
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
                    matches with <Code>{h.rule}</Code>
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

/** Whether every source is inline, so there is nothing to fetch and no "last fetched". */
export function inlineOnly(sources: ListSource[] | undefined): boolean {
  return !!sources?.length && sources.every((s) => !s.url && !s.configMapRef);
}

/** How long the page polls list status after a create or refresh. */
export const STATUS_POLL_MS = 20_000;

/** The sources one by one, a line each, for the Sources cell's tooltip. */
function describeSources(sources: ListSource[]): string[] {
  return sources.map((s) => {
    const what = s.url ?? (s.configMapRef ? `ConfigMap ${s.configMapRef.name}` : `${s.inline?.length ?? 0} inline entries`);
    return sourceEnabled(s) ? what : `${what} (off)`;
  });
}

export default function ListsPage() {
  const api = useApi();
  const can = useCan();
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
      headerActions={<Button label="Add domains" variant="secondary" isDisabled={!can('blocklists', 'write')} tooltip={can('blocklists', 'write') ? 'Add to the Allowlist or Denylist' : 'Needs blocklists.write'} onClick={() => setAdding(true)} />}
      rowActions={(l) =>
        l.spec.sources.some((s) => s.url && sourceEnabled(s)) ? (
          <Button label="Refresh" size="sm" variant="ghost" isDisabled={!can('blocklists', 'refresh')} isLoading={refresh.isPending && refresh.variables === l} onClick={() => refresh.mutate(l)} />
        ) : null
      }
      columns={[
        { header: 'Mode', cell: (l) => <Badge variant={l.spec.mode === 'allow' ? 'success' : 'error'} label={l.spec.mode ?? 'block'} /> },
        {
          header: 'Sources',
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
          width: proportional(1.5, { minWidth: 128 }),
          cell: (l) => {
            const h = listHealth(l);
            return (
              <Tooltip content={h.detail} isEnabled={!!h.detail} hasHoverIndication={false}>
                <Badge variant={h.tone === 'neutral' ? 'neutral' : h.tone} label={h.label} />
              </Tooltip>
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
