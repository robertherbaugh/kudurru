// Upstreams: the resolver groups kudurru forwards to (upstreams.groups),
// with the live health, round-trip time and backoff of every server
// (GET /stats/upstreams servers[]) and its traffic of the last 24 hours;
// bootstrap resolvers, conditional forwarding and EDNS Client Subnet.
// Everything is a cluster setting written with PATCH /settings (a JSON
// merge patch; deleting a group sends {upstreams:{groups:{name:null}}}).
// In Kubernetes the settings ConfigMap owns them and the page is read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { Policy, SettingsBody, UpstreamItem, UpstreamServer } from '../api/types';
import { ErrorBanner, errorText, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { formatExact, formatMS, formatRelative } from '../lib/format';
import { Link } from '../lib/router';
import {
  bootstrapError,
  conditionalKeyError,
  DEFAULT_GROUP,
  ECS_MODES,
  groupNameError,
  groupNames,
  mapPatch,
  parseServers,
  serverError,
  serverLabel,
  splitServerName,
  STRATEGIES,
  timeoutError,
  upstreamsOf,
  type UpstreamGroupConfig,
  type UpstreamsConfig,
} from '../lib/upstreams';

const CLUSTER_SCOPE = { scope: 'cluster' } as const;

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

function HealthCell({ h, now }: { h: UpstreamServer | undefined; now: number }) {
  if (!h) return <span className="k-muted">no data yet</span>;
  const backoff = h.backoff_until && new Date(h.backoff_until).getTime() > now;
  if (h.healthy && !backoff) {
    return (
      <span className="k-row">
        <StatusDot variant="success" label="healthy" /> healthy
      </span>
    );
  }
  return (
    <span className="k-row">
      <StatusDot variant="error" label="in backoff" /> {backoff ? `backoff, retried ${formatRelative(h.backoff_until, now)}` : 'unhealthy'}
    </span>
  );
}

interface GroupDialogProps {
  open: boolean;
  /** The group being edited; undefined creates one. */
  name?: string;
  initial?: UpstreamGroupConfig;
  existing: string[];
  onClose: () => void;
  onSave: (name: string, g: UpstreamGroupConfig) => Promise<void>;
}

/** Validates a group form; keys are field names. */
export function groupErrors(name: string, isNew: boolean, existing: string[], servers: string[], timeout: string): Record<string, string> {
  const e: Record<string, string> = {};
  if (isNew) {
    const ne = groupNameError(name);
    if (ne) e.name = ne;
    else if (existing.includes(name)) e.name = 'A group with this name exists';
  }
  if (servers.length === 0) e.servers = 'A group needs at least one server';
  const bad = servers.map((s, i) => [i, serverError(s)] as const).filter(([, err]) => err);
  if (bad.length) {
    const [i, err] = bad[0] as readonly [number, string];
    e.servers = `Line ${i + 1} (${servers[i]}): ${err}${bad.length > 1 ? ` (and ${bad.length - 1} more)` : ''}`;
  }
  const te = timeoutError(timeout);
  if (te) e.timeout = te;
  return e;
}

function GroupDialog({ open, name, initial, existing, onClose, onSave }: GroupDialogProps) {
  const strategyId = useId();
  const isNew = name === undefined;
  const [groupName, setGroupName] = useState('');
  const [servers, setServers] = useState('');
  const [strategy, setStrategy] = useState('parallel_best');
  const [timeout, setTimeoutValue] = useState('');
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [serverErr, setServerErr] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!open) return;
    setGroupName(name ?? '');
    setServers((initial?.servers ?? []).join('\n'));
    setStrategy(initial?.strategy || 'parallel_best');
    setTimeoutValue(initial?.timeout ?? '');
    setErrors({});
    setServerErr(undefined);
  }, [open, name, initial]);
  const submit = async () => {
    const list = parseServers(servers);
    const n = isNew ? groupName.trim() : (name ?? '');
    const e = groupErrors(n, isNew, existing, list, timeout);
    setErrors(e);
    if (Object.keys(e).length) return;
    setBusy(true);
    setServerErr(undefined);
    try {
      await onSave(n, { servers: list, strategy, timeout: timeout.trim() || '2s' });
      onClose();
    } catch (err) {
      setServerErr(errorText(err));
    } finally {
      setBusy(false);
    }
  };
  const help = STRATEGIES.find((s) => s.value === strategy)?.help;
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={isNew ? 'New upstream group' : `Edit upstream group ${name}`}
      width={600}
      noValidate
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label={isNew ? 'Create group' : 'Save'} variant="primary" isLoading={busy} />
        </>
      }
    >
      {isNew ? (
        <TextInput
          label="Name"
          value={groupName}
          onChange={setGroupName}
          placeholder="corp"
          description="Policies and forward zones refer to the group by this name"
          isRequired
          status={errors.name ? { type: 'error', message: errors.name } : undefined}
        />
      ) : null}
      <TextArea
        label="Servers"
        description="One per line: 192.0.2.53, 192.0.2.53:5353, tls://dns.quad9.net, https://dns.google/dns-query, quic://dns.adguard-dns.com, h3://…, sdns://…; a display name may go first, as in cloudflare=tls://1.1.1.1"
        value={servers}
        onChange={setServers}
        rows={6}
        hasSpellCheck={false}
        placeholder={'1.1.1.1:53\nquad9=tls://dns.quad9.net'}
        status={errors.servers ? { type: 'error', message: errors.servers } : undefined}
      />
      <label className="k-label" htmlFor={strategyId}>
        Strategy
        <select id={strategyId} className="k-native-select" value={strategy} onChange={(e) => setStrategy(e.target.value)}>
          {STRATEGIES.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
        {help ? <span className="k-muted k-small">{help}</span> : null}
      </label>
      <TextInput
        label="Timeout"
        value={timeout}
        onChange={setTimeoutValue}
        placeholder="2s"
        description="Per server and query; empty is 2s"
        status={errors.timeout ? { type: 'error', message: errors.timeout } : undefined}
      />
      {serverErr ? <Banner status="error" title="The server rejected the change" description={serverErr} /> : null}
    </FormDialog>
  );
}

function GroupPanel({
  name,
  g,
  servers,
  traffic,
  canWrite,
  onEdit,
  onDelete,
}: {
  name: string;
  g: UpstreamGroupConfig;
  servers: UpstreamServer[] | undefined;
  traffic: UpstreamItem[] | null | undefined;
  canWrite: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const now = Date.now();
  const titleId = useId();
  return (
    <section className="k-panel" aria-labelledby={titleId} data-testid={`group-${name}`}>
      <div className="k-panel-title">
        <h2 id={titleId} className="k-mono">
          {name}
        </h2>
        {name === DEFAULT_GROUP ? <Badge variant="neutral" label="default" /> : null}
        <Badge variant="blue" label={strategyLabel(g.strategy)} />
        <span className="k-muted k-small">timeout {g.timeout || '2s'}</span>
        <span className="k-spacer" />
        <Button label="Edit" size="sm" variant="secondary" isDisabled={!canWrite} onClick={onEdit} />
        <Button
          label="Delete"
          size="sm"
          variant="ghost"
          isDisabled={!canWrite || name === DEFAULT_GROUP}
          tooltip={name === DEFAULT_GROUP ? 'The default group is required' : undefined}
          onClick={onDelete}
        />
      </div>
      <div className="k-table-wrap">
        <table className="k-table" aria-label={`Servers of ${name}`}>
          <thead>
            <tr>
              <th scope="col">Server</th>
              <th scope="col">Health</th>
              <th scope="col" className="num">
                RTT
              </th>
              <th scope="col" className="num">
                Failures in a row
              </th>
              <th scope="col" className="num">
                Queries (24 h)
              </th>
              <th scope="col" className="num">
                Avg / p95
              </th>
              <th scope="col" className="num">
                Errors
              </th>
            </tr>
          </thead>
          <tbody>
            {g.servers.map((entry) => {
              const { name: display, addr } = splitServerName(entry);
              const h = healthFor(servers, name, entry);
              const t = trafficFor(traffic, name, entry);
              return (
                <tr key={entry}>
                  <td>
                    {display ? <strong>{display} </strong> : null}
                    <span className="k-mono k-small k-break">{addr}</span>
                  </td>
                  <td>
                    <HealthCell h={h} now={now} />
                  </td>
                  <td className="num">{h && h.rtt_ms > 0 ? formatMS(h.rtt_ms) : '–'}</td>
                  <td className="num">{h ? formatExact(h.consecutive_failures) : '–'}</td>
                  <td className="num">
                    {t ? <Link href={`/queries?upstream=${encodeURIComponent(t.addr)}`}>{formatExact(t.total)}</Link> : '0'}
                  </td>
                  <td className="num">{t ? `${formatMS(t.avg_ms)} / ${formatMS(t.p95_ms)}` : '–'}</td>
                  <td className="num">{t ? formatExact(t.errors + t.timeouts) : '–'}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}

interface Rule {
  suffix: string;
  group: string;
}

/** Validates the conditional rules; returns one message per bad row index. */
export function ruleErrors(rules: Rule[], groups: string[]): Record<number, string> {
  const e: Record<number, string> = {};
  const seen = new Set<string>();
  rules.forEach((r, i) => {
    const ke = conditionalKeyError(r.suffix);
    if (ke) e[i] = ke;
    else if (seen.has(r.suffix.trim())) e[i] = 'Listed twice';
    else if (!groups.includes(r.group)) e[i] = `No group named "${r.group}"`;
    seen.add(r.suffix.trim());
  });
  return e;
}

function ConditionalPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: (patch: Record<string, unknown>, what: string) => Promise<void> }) {
  const titleId = useId();
  const names = groupNames(cfg);
  const initial = useMemo(() => Object.entries(cfg.conditional).map(([suffix, group]) => ({ suffix, group })), [cfg.conditional]);
  const [rules, setRules] = useState<Rule[]>(initial);
  const [errors, setErrors] = useState<Record<number, string>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string>();
  useEffect(() => setRules(initial), [initial]);
  const dirty = JSON.stringify(rules) !== JSON.stringify(initial);
  const save = async () => {
    const e = ruleErrors(rules, names);
    setErrors(e);
    if (Object.keys(e).length) return;
    const after = Object.fromEntries(rules.map((r) => [r.suffix.trim(), r.group]));
    setBusy(true);
    setErr(undefined);
    try {
      await onSave({ conditional: mapPatch(cfg.conditional, after) }, 'Conditional forwarding');
    } catch (x) {
      setErr(errorText(x));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="k-panel" aria-labelledby={titleId}>
      <h2 id={titleId} className="k-h3">
        Conditional forwarding
      </h2>
      <p className="k-muted k-small">
        Queries under a domain suffix go to a group (the longest suffix wins; answers are not cached). A rule may list several suffixes in dnsmasq form,{' '}
        <code>[/corp.example/10.in-addr.arpa/]</code>.
      </p>
      {rules.length === 0 ? <p className="k-muted">No rules; everything not matched by a policy or zone uses the default group.</p> : null}
      {rules.map((r, i) => (
        <div key={i} className="k-row-end">
          <TextInput
            label="Domain suffix"
            isLabelHidden={i > 0}
            value={r.suffix}
            onChange={(v) => setRules(rules.map((x, j) => (j === i ? { ...x, suffix: v } : x)))}
            placeholder="corp.example"
            isDisabled={!canWrite}
            status={errors[i] ? { type: 'error', message: errors[i] } : undefined}
          />
          <label className="k-label">
            {i === 0 ? 'Group' : <span className="k-visually-hidden">Group</span>}
            <select className="k-native-select" value={r.group} disabled={!canWrite} onChange={(e) => setRules(rules.map((x, j) => (j === i ? { ...x, group: e.target.value } : x)))}>
              {(names.includes(r.group) ? names : [...names, r.group]).map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
          <Button label="Remove" size="sm" variant="ghost" isDisabled={!canWrite} onClick={() => setRules(rules.filter((_, j) => j !== i))} />
        </div>
      ))}
      {err ? <Banner status="error" title="The server rejected the change" description={err} /> : null}
      <div className="k-row">
        <Button label="Add rule" size="sm" variant="secondary" isDisabled={!canWrite} onClick={() => setRules([...rules, { suffix: '', group: names[0] ?? DEFAULT_GROUP }])} />
        {dirty ? (
          <>
            <Button label="Save rules" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
            <Button label="Reset" size="sm" variant="ghost" onClick={() => setRules(initial)} />
          </>
        ) : null}
      </div>
    </section>
  );
}

function BootstrapPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: (patch: Record<string, unknown>, what: string) => Promise<void> }) {
  const titleId = useId();
  const shown = cfg.bootstrap.join('\n');
  const [text, setText] = useState(shown);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => setText(shown), [shown]);
  const save = async () => {
    const list = parseServers(text);
    const bad = list.find((b) => bootstrapError(b));
    if (bad) {
      setError(`${bad}: ${bootstrapError(bad)}`);
      return;
    }
    setError(undefined);
    setBusy(true);
    try {
      await onSave({ bootstrap: list }, 'Bootstrap resolvers');
    } catch (x) {
      setError(errorText(x));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="k-panel" aria-labelledby={titleId}>
      <h2 id={titleId} className="k-h3">
        Bootstrap resolvers
      </h2>
      <TextArea
        label="Resolvers"
        description="Plain DNS resolvers (ip[:port]) used to look up the host names of DoT, DoH and DoQ servers; empty uses the system resolver"
        value={text}
        onChange={setText}
        rows={3}
        hasSpellCheck={false}
        isDisabled={!canWrite}
        status={error ? { type: 'error', message: error } : undefined}
      />
      {text !== shown ? (
        <div className="k-row">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
          <Button label="Reset" size="sm" variant="ghost" onClick={() => setText(shown)} />
        </div>
      ) : null}
    </section>
  );
}

function EcsPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: (patch: Record<string, unknown>, what: string) => Promise<void> }) {
  const titleId = useId();
  const modeId = useId();
  const [mode, setMode] = useState(cfg.ecs.mode);
  const [v4, setV4] = useState(String(cfg.ecs.ipv4Prefix));
  const [v6, setV6] = useState(String(cfg.ecs.ipv6Prefix));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string>();
  useEffect(() => {
    setMode(cfg.ecs.mode);
    setV4(String(cfg.ecs.ipv4Prefix));
    setV6(String(cfg.ecs.ipv6Prefix));
  }, [cfg.ecs]);
  const dirty = mode !== cfg.ecs.mode || v4 !== String(cfg.ecs.ipv4Prefix) || v6 !== String(cfg.ecs.ipv6Prefix);
  const save = async () => {
    const e: Record<string, string> = {};
    if (!/^\d+$/.test(v4) || Number(v4) > 32) e.v4 = '0-32';
    if (!/^\d+$/.test(v6) || Number(v6) > 128) e.v6 = '0-128';
    setErrors(e);
    if (Object.keys(e).length) return;
    setBusy(true);
    setErr(undefined);
    try {
      await onSave({ ecs: { mode, ipv4Prefix: Number(v4), ipv6Prefix: Number(v6) } }, 'EDNS Client Subnet');
    } catch (x) {
      setErr(errorText(x));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="k-panel" aria-labelledby={titleId}>
      <h2 id={titleId} className="k-h3">
        EDNS Client Subnet
      </h2>
      <p className="k-muted k-small">Whether upstreams learn which network a query came from (RFC 7871). Strip is the privacy default.</p>
      <label className="k-label" htmlFor={modeId}>
        Mode
        <select id={modeId} className="k-native-select" value={mode} disabled={!canWrite} onChange={(e) => setMode(e.target.value)}>
          {ECS_MODES.map((m) => (
            <option key={m.value} value={m.value}>
              {m.label}
            </option>
          ))}
        </select>
      </label>
      {mode !== 'strip' ? (
        <div className="k-row-end">
          <TextInput label="IPv4 prefix" value={v4} onChange={setV4} isDisabled={!canWrite} description="Longest prefix sent (0-32)" status={errors.v4 ? { type: 'error', message: errors.v4 } : undefined} />
          <TextInput label="IPv6 prefix" value={v6} onChange={setV6} isDisabled={!canWrite} description="Longest prefix sent (0-128)" status={errors.v6 ? { type: 'error', message: errors.v6 } : undefined} />
        </div>
      ) : null}
      {err ? <Banner status="error" title="The server rejected the change" description={err} /> : null}
      {dirty ? (
        <div className="k-row">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
        </div>
      ) : null}
    </section>
  );
}

export default function UpstreamsPage() {
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
  const [editing, setEditing] = useState<{ name?: string } | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [managed, setManaged] = useState<string | null>(null);
  const [notPersisted, setNotPersisted] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string>();
  const [deleteBusy, setDeleteBusy] = useState(false);
  // What still names a group: a missing group answers SERVFAIL, so deletes list them.
  const policies = useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies'), enabled: deleting !== null });
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list(), enabled: deleting !== null });
  const users = deleting
    ? [
        ...(policies.data?.items ?? []).filter((p) => p.spec.upstreamGroup === deleting).map((p) => `policy ${p.metadata.name}`),
        ...(zones.data?.items ?? []).filter((z) => z.upstreamGroup === deleting).map((z) => `zone ${z.zoneName}`),
      ]
    : [];
  const cfg = useMemo(() => upstreamsOf(settings.data), [settings.data]);
  const names = groupNames(cfg);
  const kube = cluster.data?.driver === 'kube';
  const readOnlyReason = managed ?? (kube ? 'In Kubernetes the kudurru-settings ConfigMap owns the cluster settings: set config.settings.upstreams in the Helm values (or edit the ConfigMap); every pod reloads it without a restart.' : null);
  const canWrite = can('settings', 'write') && !readOnlyReason;

  /** PATCH {upstreams: patch}; a 409 managed_by makes the page read-only. */
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

  const remove = async (name: string) => {
    const used = Object.entries(cfg.conditional).filter(([, g]) => g === name);
    if (used.length) {
      setDeleteErr(`Conditional forwarding still sends ${used.map(([s]) => s).join(', ')} to ${name}; change those rules first.`);
      return;
    }
    setDeleteBusy(true);
    setDeleteErr(undefined);
    try {
      await save({ groups: { [name]: null } }, `Group ${name} deleted;`);
      setDeleting(null);
    } catch (err) {
      setDeleteErr(errorText(err));
    } finally {
      setDeleteBusy(false);
    }
  };

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Upstreams</h1>
          <p className="k-muted k-small">
            The resolvers kudurru forwards to. Policies and forward zones pick a group by name; everything else uses <code>default</code>. Health is this
            node's view right now; traffic covers the last 24 hours.
          </p>
        </div>
        <div className="k-actions">
          <Button label="New group" variant="primary" isDisabled={!canWrite} tooltip={can('settings', 'write') ? undefined : 'Needs settings.write'} onClick={() => setEditing({})} />
        </div>
      </div>
      {readOnlyReason ? <Banner status="info" title="Read-only here" description={readOnlyReason} /> : null}
      {!readOnlyReason && !can('settings', 'write') ? <Banner status="info" title="Read-only" description="Changing upstreams needs the settings.write permission." /> : null}
      {notPersisted ? (
        <Banner
          status="warning"
          title="Kept in memory only"
          description="This server has no settings file, so the change is lost on restart. Set KUDURRU_SETTINGS_FILE to persist changes made here."
          isDismissable
          onDismiss={() => setNotPersisted(false)}
        />
      ) : null}
      {settings.error ? <ErrorBanner error={settings.error} /> : null}
      {stats.error ? <ErrorBanner error={stats.error} title="Upstream health is unavailable" /> : null}
      {settings.isLoading ? <Loading /> : null}
      {settings.data ? (
        <>
          {names.length === 0 ? <Banner status="warning" title="No upstream groups" description="Without a default group nothing can be forwarded." /> : null}
          {names.map((n) => {
            const g = cfg.groups[n];
            return g ? (
              <GroupPanel
                key={n}
                name={n}
                g={g}
                servers={stats.data?.servers}
                traffic={stats.data?.upstreams}
                canWrite={canWrite}
                onEdit={() => setEditing({ name: n })}
                onDelete={() => {
                  setDeleteErr(undefined);
                  setDeleting(n);
                }}
              />
            ) : null;
          })}
          <div className="k-grid k-grid-2">
            <BootstrapPanel cfg={cfg} canWrite={canWrite} onSave={save} />
            <EcsPanel cfg={cfg} canWrite={canWrite} onSave={save} />
          </div>
          <ConditionalPanel cfg={cfg} canWrite={canWrite} onSave={save} />
        </>
      ) : null}
      <GroupDialog
        open={editing !== null}
        {...(editing?.name !== undefined ? { name: editing.name } : {})}
        {...(editing?.name !== undefined && cfg.groups[editing.name] ? { initial: cfg.groups[editing.name] } : {})}
        existing={names}
        onClose={() => setEditing(null)}
        onSave={(name, g) => save({ groups: { [name]: g } }, `Group ${name}`)}
      />
      <FormDialog
        isOpen={deleting !== null}
        onClose={() => setDeleting(null)}
        title={`Delete upstream group ${deleting ?? ''}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setDeleting(null)} />
            <Button label="Delete" variant="destructive" isLoading={deleteBusy} onClick={() => deleting && void remove(deleting)} />
          </>
        }
      >
        <p>Queries routed to a group that does not exist fail with SERVFAIL, so change the policies and forward zones that name it first.</p>
        {users.length ? (
          <Banner status="warning" title="Still in use" description={`Named by ${users.join(', ')}.`} />
        ) : policies.isLoading || zones.isLoading ? null : (
          <p className="k-muted k-small">No policy or forward zone names this group.</p>
        )}
        {deleteErr ? <Banner status="error" title="The group was not deleted" description={deleteErr} /> : null}
      </FormDialog>
    </div>
  );
}
