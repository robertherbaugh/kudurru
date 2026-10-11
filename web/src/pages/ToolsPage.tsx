// Tools (§10 principle 13): the built-in dig (POST /dns/query runs the
// whole pipeline in-process), cache flush, YAML/JSON import through
// POST /apply, backup/export downloads and the doctor report (GET /doctor,
// the server-side checks of `kudurru doctor`, plus the cluster nodes and
// the query-log status).
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { ApplyResult, DnsProto, DnsQueryResult } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { formatBytes, formatDateTime, formatExact, formatMS } from '../lib/format';
import { Link } from '../lib/router';
import { hostError, splitPresentation } from '../lib/rr';

const DIG_TYPES = ['A', 'AAAA', 'HTTPS', 'CNAME', 'MX', 'TXT', 'NS', 'SOA', 'SRV', 'PTR', 'CAA', 'SVCB', 'DS', 'DNSKEY'];
const DIG_PROTOS: { value: DnsProto; label: string }[] = [
  { value: 'udp', label: 'UDP' },
  { value: 'tcp', label: 'TCP' },
  { value: 'dot', label: 'DoT' },
  { value: 'doh', label: 'DoH' },
  { value: 'doq', label: 'DoQ' },
  { value: 'doh3', label: 'DoH3' },
];

/** One answer section: each presentation line split into its columns. */
export function DigSection({ rows, title }: { rows: string[] | null | undefined; title: string }) {
  if (!rows?.length) return null;
  return (
    <div className="k-stack">
      <p className="k-small k-muted">;; {title} SECTION:</p>
      <div className="k-table-wrap">
        <table className="k-table k-mono k-small" aria-label={`${title.toLowerCase()} section`}>
          <tbody>
            {rows.map((line, i) => {
              const rr = splitPresentation(line);
              return rr ? (
                <tr key={i}>
                  <td className="k-break">{rr.name}</td>
                  <td className="num">{rr.ttl}</td>
                  <td>{rr.class}</td>
                  <td>{rr.type}</td>
                  <td className="k-break">{rr.data}</td>
                </tr>
              ) : (
                <tr key={i}>
                  <td colSpan={5} className="k-break">
                    {line}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** The header flags of an answer that are set, in dig's order. */
export function setFlags(r: DnsQueryResult): string[] {
  const f = r.flags;
  return (['aa', 'tc', 'rd', 'ra', 'ad', 'cd'] as const).filter((k) => f?.[k]);
}

function Dig() {
  const api = useApi();
  const typeId = useId();
  const protoId = useId();
  const [name, setName] = useState('');
  const [type, setType] = useState('A');
  const [proto, setProto] = useState<DnsProto>('udp');
  const [dnssec, setDnssec] = useState(false);
  const [client, setClient] = useState('');
  const [err, setErr] = useState<string>();
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
    <section className="k-panel" aria-labelledby="dig-title">
      <h2 id="dig-title" className="k-h3">
        Query (dig)
      </h2>
      <p className="k-muted k-small">
        Runs the query through the full pipeline (policy, blocking, zones, cache, upstreams) as the given client and transport, without touching the network
        listener.
      </p>
      <form
        className="k-row-end"
        onSubmit={(e) => {
          e.preventDefault();
          const he = hostError(name);
          setErr(he ?? undefined);
          if (!he) m.mutate();
        }}
      >
        <TextInput label="Name" value={name} onChange={setName} placeholder="example.com" status={err ? { type: 'error', message: err } : undefined} />
        <label className="k-label" htmlFor={typeId}>
          Type
          <select id={typeId} className="k-native-select" value={type} onChange={(e) => setType(e.target.value)}>
            {DIG_TYPES.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </label>
        <label className="k-label" htmlFor={protoId}>
          Transport
          <select id={protoId} className="k-native-select" value={proto} onChange={(e) => setProto(e.target.value as DnsProto)}>
            {DIG_PROTOS.map((p) => (
              <option key={p.value} value={p.value}>
                {p.label}
              </option>
            ))}
          </select>
        </label>
        <TextInput label="As client" value={client} onChange={setClient} placeholder="192.168.1.30" isOptional />
        <CheckboxInput label="DNSSEC (DO bit)" value={dnssec} onChange={setDnssec} />
        <Button type="submit" label="Query" variant="primary" isLoading={m.isPending} />
      </form>
      {m.error ? <ErrorBanner error={m.error} /> : null}
      {r ? (
        <div className="k-stack" aria-live="polite" data-testid="dig-result">
          <p className="k-row">
            <Badge variant={r.status_class === 'blocked' ? 'error' : r.status_class === 'error' ? 'warning' : 'success'} label={r.status.replace(/_/g, ' ')} />
            <span className="k-mono">{r.rcode}</span>
            <span className="k-mono k-small">
              {r.name} {r.type}
            </span>
            <span className="k-muted k-small">{formatMS(r.latency_ms)} in the pipeline</span>
            {flags.length ? <span className="k-mono k-small">flags: {flags.join(' ')}</span> : null}
            {r.client ? <span className="k-muted k-small">as {r.client}</span> : null}
            {r.node ? <span className="k-muted k-small">node {r.node}</span> : null}
          </p>
          {r.error ? <Banner status="warning" title="The pipeline reported an error" description={r.error} /> : null}
          <dl className="k-why-grid">
            {r.list ? (
              <div>
                <dt>Matched list</dt>
                <dd>{r.list}</dd>
              </div>
            ) : null}
            {r.rule ? (
              <div>
                <dt>Rule</dt>
                <dd className="k-mono">{r.rule}</dd>
              </div>
            ) : null}
            {r.upstream ? (
              <div>
                <dt>Upstream</dt>
                <dd>{r.upstream}</dd>
              </div>
            ) : null}
          </dl>
          <DigSection title="ANSWER" rows={r.answer} />
          <DigSection title="AUTHORITY" rows={r.authority} />
          <DigSection title="ADDITIONAL" rows={r.additional} />
          {!r.answer?.length && !r.authority?.length ? <p className="k-muted">No records in the answer.</p> : null}
        </div>
      ) : null}
    </section>
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
    <section className="k-panel" aria-labelledby="flush-title">
      <h2 id="flush-title" className="k-h3">
        Response cache
      </h2>
      <form
        className="k-row-end"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <TextInput label="Name" description="Empty flushes everything" value={name} onChange={setName} placeholder="example.com" isOptional />
        <Button type="submit" label={name.trim() ? 'Flush name' : 'Flush all'} variant="secondary" isDisabled={!can('cache', 'flush')} isLoading={m.isPending} />
      </form>
    </section>
  );
}

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
    <section className="k-panel" aria-labelledby="apply-title">
      <h2 id="apply-title" className="k-h3">
        Import objects
      </h2>
      <p className="k-muted k-small">
        Paste multi-document YAML or a JSON array of kudurru objects (what <code>kudurru apply -f</code> sends). Objects are applied whole, managed by
        the API. For BIND zone files use Import on a <Link href="/zones">zone</Link>.
      </p>
      <form
        className="k-stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (text.trim()) m.mutate();
        }}
      >
        <TextArea label="Objects" value={text} onChange={setText} rows={10} hasSpellCheck={false} placeholder={'apiVersion: kudurru.io/v1alpha1\nkind: BlockList\nmetadata:\n  name: example\n  namespace: default\nspec:\n  sources:\n    - url: https://example.com/hosts'} />
        <div>
          <Button type="submit" label="Apply" variant="primary" isDisabled={!text.trim() || !can('blocklists', 'write')} isLoading={m.isPending} />
        </div>
      </form>
      {m.error ? <ErrorBanner error={m.error} /> : null}
      {results ? (
        <table className="k-table" aria-label="Apply results">
          <thead>
            <tr>
              <th scope="col">Kind</th>
              <th scope="col">Name</th>
              <th scope="col">Result</th>
            </tr>
          </thead>
          <tbody>
            {results.map((r) => (
              <tr key={`${r.kind}/${r.namespace ?? ''}/${r.name}`}>
                <td>{r.kind}</td>
                <td className="k-mono">{r.namespace ? `${r.namespace}/${r.name}` : r.name}</td>
                <td>
                  <Badge variant={r.result === 'error' ? 'error' : r.result === 'unchanged' ? 'neutral' : 'success'} label={r.result} />
                  {r.error ? <span className="k-error-text"> {r.error}</span> : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </section>
  );
}

function Exports() {
  const api = useApi();
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  const kinds = ['dnszones', 'dnsrecordsets', 'blocklists', 'clientgroups', 'policies'] as const;
  return (
    <section className="k-panel" aria-labelledby="exp-title">
      <h2 id="exp-title" className="k-h3">
        Backup and export
      </h2>
      <p className="k-muted k-small">
        A full backup (state.db, keys, query log) is taken on the host with <code>kudurru backup</code>. From here you can download the objects as
        JSON and each zone as a BIND file.
      </p>
      <ul className="k-link-list">
        {kinds.map((k) => (
          <li key={k}>
            <a href={api.objectsURL(k)} download={`${k}.json`}>
              {k}.json
            </a>
          </li>
        ))}
        {(zones.data?.items ?? []).map((z) => (
          <li key={z.name}>
            <a href={api.zones.exportURL(z.name)} download={`${z.name}.zone`}>
              {z.name}.zone
            </a>
          </li>
        ))}
        <li>
          <a href={api.audit.exportURL()} download="audit.jsonl">
            audit log
          </a>
        </li>
      </ul>
      <p className="k-small k-muted">
        The dashboard includes open-source software; see the <a href="third-party-licenses.txt">third-party licences</a>.
      </p>
    </section>
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
    <section className="k-panel" aria-labelledby="doc-title">
      <h2 id="doc-title" className="k-h3">
        Doctor
      </h2>
      <p className="k-muted k-small">
        Checks what the API can see. <code>kudurru doctor</code> on the host also checks ports, file permissions and the data directory.
      </p>
      <div>
        <Button label={lines ? 'Run again' : 'Run checks'} variant="secondary" isLoading={m.isPending} onClick={() => m.mutate()} />
      </div>
      {lines ? (
        <>
          {lines.some((l) => l.ok === false) ? <Banner status="error" title="Some checks failed" /> : <Banner status="success" title="All checks passed" />}
          <pre className="k-pre" data-testid="doctor-output">
            {text}
          </pre>
        </>
      ) : null}
    </section>
  );
}

export default function ToolsPage() {
  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Tools</h1>
      </div>
      <Dig />
      <div className="k-grid k-grid-2">
        <FlushCache />
        <Doctor />
      </div>
      <ApplyYAML />
      <Exports />
    </div>
  );
}
