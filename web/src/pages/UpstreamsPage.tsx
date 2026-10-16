// Upstreams: the resolver groups kudurru forwards to or resolves with
// recursively (upstreams.groups) as a table whose rows open the group's
// page (/upstreams/:group) with its servers' live health and traffic; new
// groups are created with the wizard in a modal (?new=1). Bootstrap
// resolvers, EDNS Client Subnet, the recursive resolver's options
// (upstreams.recursion) and conditional forwarding are edited here. Everything is a cluster setting written
// with PATCH /settings (a JSON merge patch); in Kubernetes the settings
// ConfigMap owns them and the pages are read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Code } from '@astryxdesign/core/Code';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { pixel, proportional, Table, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useEffect, useMemo, useState } from 'react';
import type { UpstreamItem, UpstreamServer } from '../api/types';
import { ErrorBanner, errorText, Loading } from '../components/Feedback';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { EnumSelect } from '../components/fields/EnumSelect';
import { NumberField, numberError } from '../components/fields/NumberField';
import { cleanList, StringListField } from '../components/fields/StringListField';
import { TextField } from '../components/fields/TextField';
import { SwitchField } from '../components/fields/TextField';
import { healthSummary, resolutionLabel, trafficFor, useUpstreams } from '../components/UpstreamGroup';
import { UpstreamWizardDialog } from '../components/wizards/UpstreamWizard';
import { formatExact } from '../lib/format';
import { useNewParam } from '../lib/router';
import {
  bootstrapError,
  conditionalKeyError,
  DEFAULT_GROUP,
  ECS_MODES,
  groupNames,
  mapPatch,
  RECURSION_IPV6,
  serverEntries,
  timeoutOf,
  type UpstreamGroupConfig,
  type UpstreamsConfig,
} from '../lib/upstreams';

export { healthFor, strategyLabel, trafficFor } from '../components/UpstreamGroup';

type Save = (patch: Record<string, unknown>, what: string) => Promise<void>;

interface GroupRow extends Record<string, unknown> {
  name: string;
  g: UpstreamGroupConfig;
  healthy: number;
  known: number;
  queries: number;
}

/** data-testid="group-<name>" on each row (tests and e2e find groups by it). */
const groupRowAttrs: TablePlugin<GroupRow> = {
  transformBodyRow: (props, item) => ({ ...props, htmlProps: { ...props.htmlProps, ...({ 'data-testid': `group-${item.name}` } as Record<string, string>) } }),
};

const GROUP_COLUMNS: TableColumn<GroupRow>[] = [
  {
    key: 'name',
    header: 'Group',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (r) => (
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Link href={`/upstreams/${encodeURIComponent(r.name)}`} color="inherit" hasUnderline>
          <Text type="code" size="sm">{r.name}</Text>
        </Link>
        {r.name === DEFAULT_GROUP ? <Badge variant="neutral" label="default" /> : null}
      </HStack>
    ),
  },
  { key: 'strategy', header: 'Strategy', width: proportional(2, { minWidth: 140 }), renderCell: (r) => <Badge variant="blue" label={resolutionLabel(r.g)} /> },
  {
    key: 'servers',
    header: 'Servers',
    align: 'end',
    width: pixel(80),
    renderCell: (r) => (r.g.mode === 'recursive' ? <Text color="secondary">–</Text> : <Text hasTabularNumbers>{r.g.servers.length}</Text>),
  },
  {
    key: 'health',
    header: 'Health',
    width: proportional(2, { minWidth: 140 }),
    renderCell: (r) =>
      r.known === 0 ? (
        <Text color="secondary">no data yet</Text>
      ) : (
        <HStack gap={2} vAlign="center">
          <StatusDot variant={r.healthy === r.known ? 'success' : r.healthy === 0 ? 'error' : 'warning'} label={`${r.healthy} of ${r.known} healthy`} />
          <Text>
            {r.healthy} of {r.known} healthy
          </Text>
        </HStack>
      ),
  },
  { key: 'queries', header: 'Queries (24 h)', align: 'end', width: pixel(130), renderCell: (r) => <Text hasTabularNumbers>{formatExact(r.queries)}</Text> },
  { key: 'timeout', header: 'Timeout', width: pixel(100), renderCell: (r) => <Text>{timeoutOf(r.g)}</Text> },
];

function GroupsTable({ cfg, servers, traffic }: { cfg: UpstreamsConfig; servers: UpstreamServer[] | undefined; traffic: UpstreamItem[] | null | undefined }) {
  const rows = groupNames(cfg).flatMap((n): GroupRow[] => {
    const g = cfg.groups[n];
    if (!g) return [];
    const h = healthSummary(servers, n, serverEntries(g));
    const queries = serverEntries(g).reduce((sum, e) => sum + (trafficFor(traffic, n, e)?.total ?? 0), 0);
    return [{ name: n, g, healthy: h.healthy, known: h.known, queries }];
  });
  return (
    <Card>
      <Table<GroupRow> aria-label="Upstream groups" density="compact" hasHover idKey="name" data={rows} columns={GROUP_COLUMNS} plugins={{ groupRowAttrs }} emptyState={false} />
    </Card>
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
    <Panel
      title="Conditional forwarding"
      description={
        <>
          Queries under a domain suffix go to a group (the longest suffix wins; answers are not cached). A rule may list several suffixes in dnsmasq form,{' '}
          <Code size="inherit">[/corp.example/10.in-addr.arpa/]</Code>.
        </>
      }
    >
      {rules.length === 0 ? (
        <Text as="p" color="secondary">
          No rules; everything not matched by a policy or zone uses the default group.
        </Text>
      ) : null}
      {rules.map((r, i) => (
        <HStack key={i} gap={2} vAlign="end" wrap="wrap">
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
        </HStack>
      ))}
      {err ? <Banner status="error" title="The server rejected the change" description={err} /> : null}
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Button label="Add rule" size="sm" variant="secondary" isDisabled={!canWrite} onClick={() => setRules([...rules, { suffix: '', group: names[0] ?? DEFAULT_GROUP }])} />
        {dirty ? (
          <>
            <Button label="Save rules" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
            <Button label="Reset" size="sm" variant="ghost" onClick={() => setRules(initial)} />
          </>
        ) : null}
      </HStack>
    </Panel>
  );
}

function BootstrapPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
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
    <Panel title="Bootstrap resolvers">
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
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
          <Button label="Reset" size="sm" variant="ghost" onClick={() => setList(cfg.bootstrap)} />
        </HStack>
      ) : null}
    </Panel>
  );
}

const ECS_OPTIONS = ECS_MODES.map((m) => ({ value: m.value, label: m.label }));

function EcsPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
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
    <Panel title="EDNS Client Subnet" description="Whether upstreams learn which network a query came from (RFC 7871). Strip is the privacy default; recursive groups never send one.">
      <EnumSelect label="Mode" value={mode} options={ECS_OPTIONS} isDisabled={!canWrite} onChange={setMode} />
      {mode !== 'strip' ? (
        <HStack gap={2} vAlign="end" wrap="wrap">
          <NumberField label="IPv4 prefix" value={v4} onChange={setV4} min={0} max={32} isDisabled={!canWrite} help="Longest prefix sent" />
          <NumberField label="IPv6 prefix" value={v6} onChange={setV6} min={0} max={128} isDisabled={!canWrite} help="Longest prefix sent" />
        </HStack>
      ) : null}
      {err ? <Banner status="error" title="The server rejected the change" description={err} /> : null}
      {dirty ? (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
        </HStack>
      ) : null}
    </Panel>
  );
}

const IPV6_OPTIONS = RECURSION_IPV6.map((m) => ({ value: m.value, label: m.label, help: m.help }));

/** upstreams.recursion: what every recursive group does; trust anchors and root hints stay on the Settings page. */
function RecursionPanel({ cfg, canWrite, onSave }: { cfg: UpstreamsConfig; canWrite: boolean; onSave: Save }) {
  const [dnssec, setDnssec] = useState(cfg.recursion.dnssec);
  const [qmin, setQmin] = useState(cfg.recursion.qnameMinimization);
  const [ipv6, setIPv6] = useState(cfg.recursion.ipv6);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string>();
  useEffect(() => {
    setDnssec(cfg.recursion.dnssec);
    setQmin(cfg.recursion.qnameMinimization);
    setIPv6(cfg.recursion.ipv6);
  }, [cfg.recursion]);
  const used = Object.values(cfg.groups).some((g) => g.mode === 'recursive');
  const dirty = dnssec !== cfg.recursion.dnssec || qmin !== cfg.recursion.qnameMinimization || ipv6 !== cfg.recursion.ipv6;
  const custom = cfg.recursion.trustAnchors.length > 0 || cfg.recursion.rootHints.length > 0;
  const save = async () => {
    setBusy(true);
    setErr(undefined);
    try {
      await onSave({ recursion: { dnssec, qnameMinimization: qmin, ipv6 } }, 'Recursion');
    } catch (x) {
      setErr(errorText(x));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Panel title="Recursion" description="How groups in recursive mode resolve names from the root servers." data-testid="recursion">
      {used ? null : (
        <Text as="p" type="supporting">
          No group is recursive yet; these apply once one is.
        </Text>
      )}
      <SwitchField
        label="DNSSEC validation"
        help="Answers that fail validation become SERVFAIL (status dnssec bogus); unsigned zones still resolve."
        value={dnssec}
        onChange={setDnssec}
        isDisabled={!canWrite}
      />
      <SwitchField label="QNAME minimization" help="Each name server sees only as much of the name as it needs (RFC 9156)." value={qmin} onChange={setQmin} isDisabled={!canWrite} />
      <EnumSelect label="IPv6 to name servers" value={ipv6} options={IPV6_OPTIONS} isDisabled={!canWrite} onChange={setIPv6} />
      <Text as="p" type="supporting">
        {custom ? 'Custom root trust anchors or root hints are set' : 'Built-in root trust anchors and root hints'}; change them in{' '}
        <Link href="/settings?view=expert&q=upstreams.recursion" hasUnderline>
          Settings
        </Link>
        .
      </Text>
      {err ? <Banner status="error" title="The server rejected the change" description={err} /> : null}
      {dirty ? (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
        </HStack>
      ) : null}
    </Panel>
  );
}

export default function UpstreamsPage() {
  const [creating, setCreating] = useNewParam();
  const u = useUpstreams();
  const names = groupNames(u.cfg);
  return (
    <Page>
      <PageHeader
        title="Upstreams"
        description={
          <>
            The resolvers kudurru forwards to, or its own recursive resolver. Policies and forward zones pick a group by name; everything else uses <Code size="inherit">default</Code>. Health is this
            node's view right now; traffic covers the last 24 hours.
          </>
        }
        actions={
          <Button label="New group" variant="primary" isDisabled={!u.canWrite} tooltip={u.canWriteSetting ? undefined : 'Needs settings.write'} onClick={() => setCreating(true)} />
        }
      />
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
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <BootstrapPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
            <EcsPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
            <RecursionPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
          </Grid>
          <ConditionalPanel cfg={u.cfg} canWrite={u.canWrite} onSave={u.save} />
        </>
      ) : null}
      <UpstreamWizardDialog isOpen={creating} onClose={() => setCreating(false)} />
    </Page>
  );
}
