// Pieces shared by the Upstreams list, the group page (/upstreams/:group)
// and the new-group wizard: the typed group fields (servers as rows with
// per-entry validation, strategy as an EnumSelect with help, timeout with
// presets), the per-server health/RTT/traffic table, and the hooks that
// read and write upstreams.* with PATCH /settings.
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { SettingsBody, UpstreamItem, UpstreamServer } from '../api/types';
import { formatExact, formatMS, formatRelative } from '../lib/format';
import { serverError, serverLabel, splitServerName, STRATEGIES, timeoutError, upstreamsOf, type UpstreamGroupConfig } from '../lib/upstreams';
import { DurationField, durationPresets } from './fields/DurationField';
import { EnumSelect } from './fields/EnumSelect';
import { listError, StringListField } from './fields/StringListField';
import { useNotify } from './Feedback';

export const CLUSTER_SCOPE = { scope: 'cluster' } as const;

export const TIMEOUT_PRESETS = durationPresets(['500ms', '1s', '2s', '3s', '5s', '10s']);

/** The strategy label for a group ("" is the default, parallel_best). */
export function strategyLabel(s: string): string {
  return STRATEGIES.find((x) => x.value === (s || 'parallel_best'))?.label ?? s;
}

/** Live health of a configured server entry of a group. */
export function healthFor(servers: UpstreamServer[] | undefined, group: string, entry: string): UpstreamServer | undefined {
  const label = serverLabel(entry);
  return servers?.find((s) => s.group === group && s.server === label);
}

/** Traffic of a server over the window (the query log labels it group/server). */
export function trafficFor(items: UpstreamItem[] | null | undefined, group: string, entry: string): UpstreamItem | undefined {
  const label = `${group}/${serverLabel(entry)}`;
  return items?.find((u) => u.addr === label);
}

/** Healthy servers of a group right now, of those with data. */
export function healthSummary(servers: UpstreamServer[] | undefined, group: string, entries: string[], now = Date.now()): { healthy: number; known: number } {
  let healthy = 0;
  let known = 0;
  for (const e of entries) {
    const h = healthFor(servers, group, e);
    if (!h) continue;
    known++;
    if (h.healthy && !(h.backoff_until && new Date(h.backoff_until).getTime() > now)) healthy++;
  }
  return { healthy, known };
}

/** Validates the group fields; keys are field names. */
export function groupFieldErrors(g: UpstreamGroupConfig): Record<string, string> {
  const e: Record<string, string> = {};
  const servers = g.servers.map((s) => s.trim()).filter(Boolean);
  if (servers.length === 0) e.servers = 'A group needs at least one server';
  const le = listError(servers, serverError);
  if (le) e.servers = le;
  const te = timeoutError(g.timeout);
  if (te) e.timeout = te;
  return e;
}

export function HealthCell({ h, now }: { h: UpstreamServer | undefined; now: number }) {
  if (!h) return <Text color="secondary">no data yet</Text>;
  const backoff = h.backoff_until && new Date(h.backoff_until).getTime() > now;
  if (h.healthy && !backoff) {
    return (
      <HStack gap={2} vAlign="center">
        <StatusDot variant="success" label="healthy" />
        <Text>healthy</Text>
      </HStack>
    );
  }
  return (
    <HStack gap={2} vAlign="center">
      <StatusDot variant="error" label="in backoff" />
      <Text>{backoff ? `backoff, retried ${formatRelative(h.backoff_until, now)}` : 'unhealthy'}</Text>
    </HStack>
  );
}

interface ServerRow extends Record<string, unknown> {
  entry: string;
  display: string | undefined;
  addr: string;
  h: UpstreamServer | undefined;
  t: UpstreamItem | undefined;
}

const num = (text: string) => <Text hasTabularNumbers>{text}</Text>;

export function ServersTable({ name, g, servers, traffic }: { name: string; g: UpstreamGroupConfig; servers: UpstreamServer[] | undefined; traffic: UpstreamItem[] | null | undefined }) {
  const now = Date.now();
  const rows: ServerRow[] = g.servers.map((entry) => {
    const { name: display, addr } = splitServerName(entry);
    return { entry, display, addr, h: healthFor(servers, name, entry), t: trafficFor(traffic, name, entry) };
  });
  const columns: TableColumn<ServerRow>[] = [
    {
      key: 'entry',
      header: 'Server',
      width: proportional(3, { minWidth: 200 }),
      renderCell: (r) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          {r.display ? <Text weight="semibold">{r.display}</Text> : null}
          <Text type="code" size="sm" maxLines={2} wordBreak="break-all">
            {r.addr}
          </Text>
        </HStack>
      ),
    },
    { key: 'health', header: 'Health', width: proportional(2, { minWidth: 140 }), renderCell: (r) => <HealthCell h={r.h} now={now} /> },
    { key: 'rtt', header: 'RTT', align: 'end', width: pixel(90), renderCell: (r) => num(r.h && r.h.rtt_ms > 0 ? formatMS(r.h.rtt_ms) : '–') },
    { key: 'failures', header: 'Failures in a row', align: 'end', width: pixel(140), renderCell: (r) => num(r.h ? formatExact(r.h.consecutive_failures) : '–') },
    {
      key: 'queries',
      header: 'Queries (24 h)',
      align: 'end',
      width: pixel(130),
      renderCell: (r) =>
        r.t ? (
          <Link href={`/queries?upstream=${encodeURIComponent(r.t.addr)}`} color="inherit" hasUnderline>
            <Text hasTabularNumbers>{formatExact(r.t.total)}</Text>
          </Link>
        ) : (
          num('0')
        ),
    },
    { key: 'latency', header: 'Avg / p95', align: 'end', width: pixel(150), renderCell: (r) => num(r.t ? `${formatMS(r.t.avg_ms)} / ${formatMS(r.t.p95_ms)}` : '–') },
    { key: 'errors', header: 'Errors', align: 'end', width: pixel(80), renderCell: (r) => num(r.t ? formatExact(r.t.errors + r.t.timeouts) : '–') },
  ];
  return <Table<ServerRow> aria-label={`Servers of ${name}`} density="compact" hasHover idKey="entry" data={rows} columns={columns} />;
}

export interface GroupFieldsProps {
  value: UpstreamGroupConfig;
  onChange: (g: UpstreamGroupConfig) => void;
  errors?: Record<string, string>;
  isDisabled?: boolean;
  /** Hide the servers (the wizard edits them on their own step). */
  serversHidden?: boolean;
}

export function GroupFields({ value, onChange, errors = {}, isDisabled, serversHidden }: GroupFieldsProps) {
  return (
    <>
      {serversHidden ? null : <ServersField value={value.servers} onChange={(servers) => onChange({ ...value, servers })} error={errors.servers} isDisabled={isDisabled} />}
      <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
        <EnumSelect label="Strategy" value={value.strategy || 'parallel_best'} options={[...STRATEGIES]} onChange={(strategy) => onChange({ ...value, strategy })} isDisabled={isDisabled} />
        <DurationField
          label="Timeout"
          description="Per server and query"
          value={value.timeout || '2s'}
          onChange={(timeout) => onChange({ ...value, timeout })}
          presets={TIMEOUT_PRESETS}
          isDisabled={isDisabled}
          error={errors.timeout}
        />
      </Grid>
    </>
  );
}

export function ServersField({ value, onChange, error, isDisabled }: { value: string[]; onChange: (v: string[]) => void; error?: string | undefined; isDisabled?: boolean | undefined }) {
  return (
    <StringListField
      label="Servers"
      description="ip[:port], tls://host, https://host/dns-query, quic://host, h3://… or sdns:// stamps; a display name may go first, as in quad9=tls://dns.quad9.net"
      value={value}
      onChange={onChange}
      placeholder="tls://dns.quad9.net"
      itemError={serverError}
      addLabel="Add server"
      isDisabled={isDisabled}
      error={error}
    />
  );
}

/** The upstreams settings, who may change them and a PATCH {upstreams: …} that updates the cache. */
export function useUpstreams() {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const settings = useQuery({ queryKey: qk.settings, queryFn: () => api.settings.get() });
  const cluster = useQuery({ queryKey: qk.cluster, queryFn: () => api.cluster.info(), enabled: can('cluster', 'read') });
  const stats = useQuery({
    queryKey: qk.stats('upstreams', CLUSTER_SCOPE, '24h'),
    queryFn: () => api.stats.upstreams(CLUSTER_SCOPE, '24h'),
    refetchInterval: 10_000,
    enabled: can('stats', 'read'),
  });
  const [managed, setManaged] = useState<string | null>(null);
  const [notPersisted, setNotPersisted] = useState(false);
  const cfg = useMemo(() => upstreamsOf(settings.data), [settings.data]);
  const kube = cluster.data?.driver === 'kube';
  const readOnlyReason =
    managed ??
    (kube
      ? 'In Kubernetes the kudurru-settings ConfigMap owns the cluster settings: set config.settings.upstreams in the Helm values (or edit the ConfigMap); every pod reloads it without a restart.'
      : null);
  const canWrite = can('settings', 'write') && !readOnlyReason;

  /** PATCH {upstreams: patch}; a 409 managed_by makes the pages read-only. */
  const save = async (patch: Record<string, unknown>, what: string) => {
    let body: SettingsBody;
    try {
      body = await api.settings.patch({ upstreams: patch });
    } catch (err) {
      if (err instanceof ApiError && err.key === 'managed_by') setManaged(err.describe());
      throw err;
    }
    qc.setQueryData(qk.settings, body);
    void qc.invalidateQueries({ queryKey: qk.schema });
    void qc.invalidateQueries({ queryKey: qk.stats('upstreams') });
    setNotPersisted(body.persisted === false);
    notify.success(`${what} saved`);
  };
  return { settings, stats, cfg, readOnlyReason, canWrite, canWriteSetting: can('settings', 'write'), save, notPersisted, clearNotPersisted: () => setNotPersisted(false) };
}
