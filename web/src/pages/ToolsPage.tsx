// Tools (§10 principle 13): the built-in dig (POST /dns/query runs the
// whole pipeline in-process), cache flush, YAML/JSON import through
// POST /apply, backup/export downloads and the doctor report (GET /doctor,
// the server-side checks of `kudurru doctor`, plus the cluster nodes and
// the query-log status).
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { TextArea } from '@astryxdesign/core/TextArea';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import { useFeature } from '../api/hooks';
import type { ApplyResult, DnsProto, DnsQueryResult } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { formatBytes, formatDateTime, formatExact, formatMS } from '../lib/format';
import { hostError, splitPresentation } from '../lib/rr';
import { formatError } from '../components/fields/formats';
import { SelectField } from '../components/fields/SelectField';
import { CheckboxField, TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { MigrationImport } from '../components/wizards/MigrationWizard';

const DIG_TYPES = ['A', 'AAAA', 'HTTPS', 'CNAME', 'MX', 'TXT', 'NS', 'SOA', 'SRV', 'PTR', 'CAA', 'SVCB', 'DS', 'DNSKEY'];
const DIG_PROTOS: { value: DnsProto; label: string }[] = [
  { value: 'udp', label: 'UDP' },
  { value: 'tcp', label: 'TCP' },
  { value: 'dot', label: 'DoT' },
  { value: 'doh', label: 'DoH' },
  { value: 'doq', label: 'DoQ' },
  { value: 'doh3', label: 'DoH3' },
];

/** One line of a dig section: its columns, or the whole line (in Name) when it does not split. */
type DigRow = { id: string; name: string; ttl: string; class: string; type: string; data: string } & Record<string, unknown>;

const code = (v: string) => (
  <Text type="code" size="sm">
    {v}
  </Text>
);

/** Long names and data wrap inside their cells. */
const digColumns: TableColumn<DigRow>[] = [
  { key: 'name', header: 'Name', width: proportional(3, { minWidth: 160 }), renderCell: (r) => code(r.name) },
  { key: 'ttl', header: 'TTL', align: 'end', width: pixel(72), renderCell: (r) => code(r.ttl) },
  { key: 'class', header: 'Class', width: pixel(64), renderCell: (r) => code(r.class) },
  { key: 'type', header: 'Type', width: pixel(88), renderCell: (r) => code(r.type) },
  { key: 'data', header: 'Data', width: proportional(4, { minWidth: 160 }), renderCell: (r) => code(r.data) },
];

/** One answer section: each presentation line split into its columns. */
export function DigSection({ rows, title }: { rows: string[] | null | undefined; title: string }) {
  if (!rows?.length) return null;
  const data: DigRow[] = rows.map((line, i) => {
    const rr = splitPresentation(line);
    return rr ? { id: String(i), ...rr } : { id: String(i), name: line, ttl: '', class: '', type: '', data: '' };
  });
  return (
    <VStack gap={1}>
      <Text type="supporting" as="p">
        ;; {title} SECTION:
      </Text>
      <Table<DigRow> density="compact" aria-label={`${title.toLowerCase()} section`} idKey="id" data={data} columns={digColumns} />
    </VStack>
  );
}

/** The header flags of an answer that are set, in dig's order. */
export function setFlags(r: DnsQueryResult): string[] {
  const f = r.flags;
  return (['aa', 'tc', 'rd', 'ra', 'ad', 'cd'] as const).filter((k) => f?.[k]);
}

function Dig() {
  const api = useApi();
  const [name, setName] = useState('');
  const [type, setType] = useState('A');
  const [proto, setProto] = useState<DnsProto>('udp');
  const [dnssec, setDnssec] = useState(false);
  const [client, setClient] = useState('');
  const [err, setErr] = useState<string>();
  const [clientErr, setClientErr] = useState<string>();
  const m = useMutation({
    mutationFn: () =>
      api.runtime.dig({
        name: name.trim(),
        type,
        ...(proto !== 'udp' ? { proto } : {}),
        ...(dnssec ? { dnssec } : {}),
        ...(client.trim() ? { client: client.trim() } : {}),
      }),
  });
  const r: DnsQueryResult | undefined = m.data;
  const flags = r ? setFlags(r) : [];
  return (
    <Panel
      title="Query (dig)"
      headingId="dig-title"
      description="Runs the query through the full pipeline (policy, blocking, zones, cache, upstreams) as the given client and transport, without touching the network listener."
    >
      <VStack
        as="form"
        gap={3}
        onSubmit={(e) => {
          e.preventDefault();
          const he = hostError(name);
          const ce = formatError('ip', client);
          setErr(he ?? undefined);
          setClientErr(ce ?? undefined);
          if (!he && !ce) m.mutate();
        }}
      >
        {/* One row of fields lined up on their controls; help and errors hang below. */}
        <Grid columns={{ minWidth: 180 }} gap={3} align="start">
          <TextField label="Name" value={name} onChange={setName} placeholder="example.com" mono error={err} />
          <SelectField label="Type" value={type} options={DIG_TYPES.map((t) => ({ value: t, label: t }))} onChange={setType} />
          <SelectField label="Transport" value={proto} options={DIG_PROTOS} onChange={(v) => setProto(v as DnsProto)} />
          <TextField
            label="As client"
            help="The client IP the query is evaluated for (its groups and policies)"
            value={client}
            onChange={setClient}
            placeholder="192.168.1.30"
            mono
            isOptional
            error={clientErr}
          />
        </Grid>
        <HStack gap={4} align="center" wrap="wrap">
          <CheckboxField label="DNSSEC (DO bit)" value={dnssec} onChange={setDnssec} />
          <Button type="submit" label="Query" variant="primary" isLoading={m.isPending} />
        </HStack>
      </VStack>
      {m.error ? <ErrorBanner error={m.error} /> : null}
      {r ? (
        // The e2e specs read the result as the panel's last polite live region.
        <VStack gap={2} aria-live="polite" data-testid="dig-result">
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Badge variant={r.status_class === 'blocked' ? 'error' : r.status_class === 'error' ? 'warning' : 'success'} label={r.status.replace(/_/g, ' ')} />
            <Text type="code">{r.rcode}</Text>
            <Text type="code" size="sm">
              {r.name} {r.type}
            </Text>
            <Text type="supporting">{formatMS(r.latency_ms)} in the pipeline</Text>
            {flags.length ? (
              <Text type="code" size="sm">
                flags: {flags.join(' ')}
              </Text>
            ) : null}
            {r.client ? <Text type="supporting">as {r.client}</Text> : null}
            {r.node ? <Text type="supporting">node {r.node}</Text> : null}
          </HStack>
          {r.error ? <Banner status="warning" title="The pipeline reported an error" description={r.error} /> : null}
          {r.list || r.rule || r.upstream ? (
            <MetadataList columns="multi">
              {r.list ? <MetadataListItem label="Matched list">{r.list}</MetadataListItem> : null}
              {r.rule ? (
                <MetadataListItem label="Rule">
                  <Text type="code">{r.rule}</Text>
                </MetadataListItem>
              ) : null}
              {r.upstream ? <MetadataListItem label="Upstream">{r.upstream}</MetadataListItem> : null}
            </MetadataList>
          ) : null}
          <DigSection title="ANSWER" rows={r.answer} />
          <DigSection title="AUTHORITY" rows={r.authority} />
          <DigSection title="ADDITIONAL" rows={r.additional} />
          {!r.answer?.length && !r.authority?.length ? (
            <Text color="secondary" as="p">
              No records in the answer.
            </Text>
          ) : null}
        </VStack>
      ) : null}
    </Panel>
  );
}

function FlushCache() {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const [name, setName] = useState('');
  const m = useMutation({
    mutationFn: () => api.runtime.flushCache(name.trim() || undefined),
    onSuccess: (r) => notify.success(`Flushed ${formatExact(r.flushed)} cache entries`),
    onError: (err) => notify.error(err, 'Flush failed'),
  });
  return (
    <Panel title="Response cache" headingId="flush-title">
      <VStack
        as="form"
        gap={3}
        hAlign="start"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <TextField label="Name" help="Empty flushes everything" value={name} onChange={setName} placeholder="example.com" mono isOptional fieldWidth="100%" />
        <Button type="submit" label={name.trim() ? 'Flush name' : 'Flush all'} variant="secondary" isDisabled={!can('cache', 'flush')} isLoading={m.isPending} />
      </VStack>
    </Panel>
  );
}

type ResultRow = ApplyResult & Record<string, unknown>;

const resultKey = (r: ResultRow) => `${r.kind}/${r.namespace ?? ''}/${r.name}`;

const resultColumns: TableColumn<ResultRow>[] = [
  { key: 'kind', header: 'Kind', width: proportional(1, { minWidth: 120 }), renderCell: (r) => <Text>{r.kind}</Text> },
  { key: 'name', header: 'Name', width: proportional(2, { minWidth: 160 }), renderCell: (r) => <Text type="code">{r.namespace ? `${r.namespace}/${r.name}` : r.name}</Text> },
  {
    key: 'result',
    header: 'Result',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (r) => (
      <VStack gap={1} align="start">
        <Badge variant={r.result === 'error' ? 'error' : r.result === 'unchanged' ? 'neutral' : 'success'} label={r.result} />
        {r.error ? <FieldStatus type="error" variant="detached" message={r.error} /> : null}
      </VStack>
    ),
  },
];

function ApplyYAML() {
  const api = useApi();
  const can = useCan();
  const [text, setText] = useState('');
  const [results, setResults] = useState<ApplyResult[] | null>(null);
  const m = useMutation({
    mutationFn: () => api.objects.apply(text, text.trim().startsWith('[') ? 'application/json' : 'application/yaml'),
    onSuccess: (r) => setResults(r.items),
  });
  return (
    <Panel
      title="Import objects"
      headingId="apply-title"
      description={
        <>
          Paste multi-document YAML or a JSON array of kudurru objects (what <Code size="inherit">kudurru apply -f</Code> sends). Objects are applied whole, managed by the
          API. For BIND zone files use Import on a{' '}
          <Link href="/zones" hasUnderline>
            zone
          </Link>
          .
        </>
      }
    >
      <VStack
        as="form"
        gap={2}
        onSubmit={(e) => {
          e.preventDefault();
          if (text.trim()) m.mutate();
        }}
      >
        <TextArea label="Objects" value={text} onChange={setText} rows={10} hasSpellCheck={false} placeholder={'apiVersion: kudurru.io/v1alpha1\nkind: BlockList\nmetadata:\n  name: example\n  namespace: default\nspec:\n  sources:\n    - url: https://example.com/hosts'} />
        <HStack>
          <Button type="submit" label="Apply" variant="primary" isDisabled={!text.trim() || !can('blocklists', 'write')} isLoading={m.isPending} />
        </HStack>
      </VStack>
      {m.error ? <ErrorBanner error={m.error} /> : null}
      {results ? <Table<ResultRow> aria-label="Apply results" density="compact" idKey={resultKey} data={results as ResultRow[]} columns={resultColumns} /> : null}
    </Panel>
  );
}

function Exports() {
  const api = useApi();
  const audit = useFeature('audit');
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  const kinds = ['dnszones', 'dnsrecordsets', 'blocklists', 'clientgroups', 'policies'] as const;
  return (
    <Panel
      title="Backup and export"
      headingId="exp-title"
      description={
        <>
          A full backup (state.db, keys, query log) is taken on the host with <Code size="inherit">kudurru backup</Code>. From here you can download the objects as JSON and
          each zone as a BIND file.
        </>
      }
    >
      <HStack role="list" gap={4} wrap="wrap" aria-label="Downloads">
        {kinds.map((k) => (
          <StackItem role="listitem" key={k}>
            <Link href={api.objectsURL(k)} download={`${k}.json`} hasUnderline>
              {k}.json
            </Link>
          </StackItem>
        ))}
        {(zones.data?.items ?? []).map((z) => (
          <StackItem role="listitem" key={z.name}>
            <Link href={api.zones.exportURL(z.name)} download={`${z.name}.zone`} hasUnderline>
              {z.name}.zone
            </Link>
          </StackItem>
        ))}
        {audit.allowed ? (
          <StackItem role="listitem">
            <Link href={api.audit.exportURL()} download="audit.jsonl" hasUnderline>
              audit log
            </Link>
          </StackItem>
        ) : null}
      </HStack>
      <Text type="supporting" as="p">
        The dashboard includes open-source software; see the{' '}
        <Link href="third-party-licenses.txt" hasUnderline>
          third-party licences
        </Link>
        .
      </Text>
    </Panel>
  );
}

interface DoctorLine {
  ok: boolean | 'warn';
  text: string;
}

function Doctor() {
  const api = useApi();
  const [lines, setLines] = useState<DoctorLine[] | null>(null);
  const m = useMutation({
    mutationFn: async () => {
      const out: DoctorLine[] = [];
      const settle = async <T,>(label: string, p: Promise<T>, f: (v: T) => DoctorLine[]) => {
        try {
          out.push(...f(await p));
        } catch (e) {
          out.push({ ok: false, text: `${label}: ${(e as Error).message}` });
        }
      };
      // The server runs the checks of `kudurru doctor` it can see from
      // inside (config, api, token, diagnoses, dataDir, ready).
      await settle('doctor', api.cluster.doctor(), (d) => [
        { ok: true, text: `kudurru ${d.version} on ${d.node}` },
        ...(d.checks ?? []).map((c) => ({ ok: c.status === 'ok' ? true : c.status === 'warn' ? ('warn' as const) : false, text: `${c.check}: ${c.message}` })),
      ]);
      await settle('nodes', api.cluster.nodes(), (n) =>
        n.items.map((x) => ({
          // Peers report no readiness until scatter-gather lands.
          ok: x.ready ?? ('warn' as const),
          text: `node ${x.name}: ${x.ready === undefined ? 'readiness unknown' : x.ready ? 'ready' : 'NOT READY'}${x.leader ? ', leader' : ''}${x.version ? `, ${x.version}` : ''}`,
        })),
      );
      await settle('query log', api.queries.status(), (s) => [
        { ok: s.wal_bytes < 256 * 1024 * 1024 ? true : 'warn', text: `query log: ${formatExact(s.rows)} rows, db ${formatBytes(s.db_bytes)}, WAL ${formatBytes(s.wal_bytes)}, oldest ${formatDateTime(s.oldest_ts)}` },
      ]);
      return out;
    },
    onSuccess: setLines,
  });
  const text = lines?.map((l) => `${l.ok === true ? '[ ok ]' : l.ok === 'warn' ? '[warn]' : '[FAIL]'} ${l.text}`).join('\n') ?? '';
  return (
    <Panel
      title="Doctor"
      headingId="doc-title"
      description={
        <>
          Checks what the API can see. <Code size="inherit">kudurru doctor</Code> on the host also checks ports, file permissions and the data directory.
        </>
      }
    >
      <HStack>
        <Button label={lines ? 'Run again' : 'Run checks'} variant="secondary" isLoading={m.isPending} onClick={() => m.mutate()} />
      </HStack>
      {lines ? (
        <>
          {lines.some((l) => l.ok === false) ? <Banner status="error" title="Some checks failed" /> : <Banner status="success" title="All checks passed" />}
          <CodeBlock data-testid="doctor-output" code={text} maxHeight={480} width="100%" />
        </>
      ) : null}
    </Panel>
  );
}

export default function ToolsPage() {
  return (
    <Page>
      <PageHeader title="Tools" />
      <Dig />
      <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
        <FlushCache />
        <Doctor />
      </Grid>
      <MigrationImport />
      <ApplyYAML />
      <Exports />
    </Page>
  );
}
