// Upstreams: the resolver groups kudurru forwards to (upstreams.groups) as
// a table whose rows open the group's page (/upstreams/:group) with its
// servers' live health and traffic; new groups are created with the wizard
// in a modal (?new=1). Bootstrap resolvers, conditional forwarding and EDNS
// Client Subnet are edited here. Everything is a cluster setting written
// with PATCH /settings (a JSON merge patch); in Kubernetes the settings
// ConfigMap owns them and the pages are read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useEffect, useId, useMemo, useState } from 'react';
import type { UpstreamItem, UpstreamServer } from '../api/types';
import { ErrorBanner, errorText, Loading } from '../components/Feedback';
import { EnumSelect } from '../components/fields/EnumSelect';
import { NumberField, numberError } from '../components/fields/NumberField';
import { cleanList, StringListField } from '../components/fields/StringListField';
import { TextField } from '../components/fields/TextField';
import { healthSummary, strategyLabel, trafficFor, useUpstreams } from '../components/UpstreamGroup';
import { UpstreamWizardDialog } from '../components/wizards/UpstreamWizard';
import { formatExact } from '../lib/format';
import { Link, useNewParam } from '../lib/router';
import { bootstrapError, conditionalKeyError, DEFAULT_GROUP, ECS_MODES, groupNames, mapPatch, type UpstreamsConfig } from '../lib/upstreams';

export { healthFor, strategyLabel, trafficFor } from '../components/UpstreamGroup';

type Save = (patch: Record<string, unknown>, what: string) => Promise<void>;

function GroupsTable({ cfg, servers, traffic }: { cfg: UpstreamsConfig; servers: UpstreamServer[] | undefined; traffic: UpstreamItem[] | null | undefined }) {
  const names = groupNames(cfg);
  return (
    <div className="k-panel k-table-wrap">
      <table className="k-table" aria-label="Upstream groups">
        <thead>
          <tr>
            <th scope="col">Group</th>
            <th scope="col">Strategy</th>
            <th scope="col" className="num">
              Servers
            </th>
            <th scope="col">Health</th>
            <th scope="col" className="num">
              Queries (24 h)
            </th>
            <th scope="col">Timeout</th>
          </tr>
        </thead>
        <tbody>
          {names.map((n) => {
            const g = cfg.groups[n];
            if (!g) return null;
            const h = healthSummary(servers, n, g.servers);
            const queries = g.servers.reduce((sum, e) => sum + (trafficFor(traffic, n, e)?.total ?? 0), 0);
            return (
              <tr key={n} data-testid={`group-${n}`}>
                <td>
                  <Link href={`/upstreams/${encodeURIComponent(n)}`} className="k-mono">
                    {n}
                  </Link>{' '}
                  {n === DEFAULT_GROUP ? <Badge variant="neutral" label="default" /> : null}
                </td>
                <td>
                  <Badge variant="blue" label={strategyLabel(g.strategy)} />
                </td>
                <td className="num">{g.servers.length}</td>
                <td>
                  {h.known === 0 ? (
                    <span className="k-muted">no data yet</span>
                  ) : (
                    <span className="k-row">
                      <StatusDot variant={h.healthy === h.known ? 'success' : h.healthy === 0 ? 'error' : 'warning'} label={`${h.healthy} of ${h.known} healthy`} />
                      {h.healthy} of {h.known} healthy
                    </span>
                  )}
                </td>
                <td className="num">{formatExact(queries)}</td>
                <td>{g.timeout || '2s'}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
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

function ConditionalPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
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
  const groupOptions = (current: string) => (names.includes(current) ? names : [...names, current]).map((n) => ({ value: n, label: names.includes(n) ? n : `${n} (missing)` }));
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
          <TextField
            label="Domain suffix"
            isLabelHidden={i > 0}
            value={r.suffix}
            onChange={(v) => setRules(rules.map((x, j) => (j === i ? { ...x, suffix: v } : x)))}
            placeholder="corp.example"
            isDisabled={!canWrite}
            error={errors[i]}
          />
          <EnumSelect
            label="Group"
            isLabelHidden={i > 0}
            value={r.group}
            options={groupOptions(r.group)}
            isDisabled={!canWrite}
            onChange={(v) => setRules(rules.map((x, j) => (j === i ? { ...x, group: v } : x)))}
          />
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

function BootstrapPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
  const titleId = useId();
  const [list, setList] = useState(cfg.bootstrap);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => setList(cfg.bootstrap), [cfg.bootstrap]);
  const dirty = JSON.stringify(cleanList(list)) !== JSON.stringify(cfg.bootstrap);
  const save = async () => {
    const clean = cleanList(list);
    const bad = clean.find((b) => bootstrapError(b));
    if (bad) {
      setError(`${bad}: ${bootstrapError(bad)}`);
      return;
    }
    setError(undefined);
    setBusy(true);
    try {
      await onSave({ bootstrap: clean }, 'Bootstrap resolvers');
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
      <StringListField
        label="Resolvers"
        description="Plain DNS resolvers (ip[:port]) used to look up the host names of DoT, DoH and DoQ servers; none uses the system resolver"
        value={list}
        onChange={setList}
        placeholder="1.1.1.1:53"
        itemError={bootstrapError}
        addLabel="Add resolver"
        isDisabled={!canWrite}
        error={error}
      />
      {dirty ? (
        <div className="k-row">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
          <Button label="Reset" size="sm" variant="ghost" onClick={() => setList(cfg.bootstrap)} />
        </div>
      ) : null}
    </section>
  );
}

const ECS_OPTIONS = ECS_MODES.map((m) => ({ value: m.value, label: m.label }));

function EcsPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
  const titleId = useId();
  const [mode, setMode] = useState(cfg.ecs.mode);
  const [v4, setV4] = useState(String(cfg.ecs.ipv4Prefix));
  const [v6, setV6] = useState(String(cfg.ecs.ipv6Prefix));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string>();
  useEffect(() => {
    setMode(cfg.ecs.mode);
    setV4(String(cfg.ecs.ipv4Prefix));
    setV6(String(cfg.ecs.ipv6Prefix));
  }, [cfg.ecs]);
  const dirty = mode !== cfg.ecs.mode || v4 !== String(cfg.ecs.ipv4Prefix) || v6 !== String(cfg.ecs.ipv6Prefix);
  const save = async () => {
    if (numberError(v4, { min: 0, max: 32 }) || numberError(v6, { min: 0, max: 128 })) return;
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
      <EnumSelect label="Mode" value={mode} options={ECS_OPTIONS} isDisabled={!canWrite} onChange={setMode} />
      {mode !== 'strip' ? (
        <div className="k-row-end">
          <NumberField label="IPv4 prefix" value={v4} onChange={setV4} min={0} max={32} isDisabled={!canWrite} help="Longest prefix sent" />
          <NumberField label="IPv6 prefix" value={v6} onChange={setV6} min={0} max={128} isDisabled={!canWrite} help="Longest prefix sent" />
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
  const [creating, setCreating] = useNewParam();
  const u = useUpstreams();
  const names = groupNames(u.cfg);
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
          <Button
            label="New group"
            variant="primary"
            isDisabled={!u.canWrite}
            tooltip={u.canWriteSetting ? undefined : 'Needs settings.write'}
            onClick={() => setCreating(true)}
          />
        </div>
      </div>
      {u.readOnlyReason ? <Banner status="info" title="Read-only here" description={u.readOnlyReason} /> : null}
      {!u.readOnlyReason && !u.canWriteSetting ? <Banner status="info" title="Read-only" description="Changing upstreams needs the settings.write permission." /> : null}
      {u.notPersisted ? (
        <Banner
          status="warning"
          title="Kept in memory only"
          description="This server has no settings file, so the change is lost on restart. Set KUDURRU_SETTINGS_FILE to persist changes made here."
          isDismissable
          onDismiss={u.clearNotPersisted}
        />
      ) : null}
      {u.settings.error ? <ErrorBanner error={u.settings.error} /> : null}
      {u.stats.error ? <ErrorBanner error={u.stats.error} title="Upstream health is unavailable" /> : null}
      {u.settings.isLoading ? <Loading /> : null}
      {u.settings.data ? (
        <>
          {names.length === 0 ? <Banner status="warning" title="No upstream groups" description="Without a default group nothing can be forwarded." /> : null}
          <GroupsTable cfg={u.cfg} servers={u.stats.data?.servers} traffic={u.stats.data?.upstreams} />
          <div className="k-grid k-grid-2">
            <BootstrapPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
            <EcsPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
          </div>
          <ConditionalPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
        </>
      ) : null}
      <UpstreamWizardDialog isOpen={creating} onClose={() => setCreating(false)} />
    </div>
  );
}
