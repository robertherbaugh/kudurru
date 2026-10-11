// Tools (§10 principle 13): the built-in dig (POST /dns/query runs the
// whole pipeline in-process), cache flush, YAML/JSON import through
// POST /apply, backup/export downloads and a doctor report assembled
// from the health, version, query-log and diagnosis endpoints.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { ApplyResult, DnsQueryResult, DnsRR } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { edeText } from '../lib/ede';
import { formatBytes, formatDateTime, formatExact, formatMS } from '../lib/format';
import { Link } from '../lib/router';
import { hostError } from '../lib/rr';

const DIG_TYPES = ['A', 'AAAA', 'HTTPS', 'CNAME', 'MX', 'TXT', 'NS', 'SOA', 'SRV', 'PTR', 'CAA', 'SVCB', 'DS', 'DNSKEY'];

function Section({ rows, title }: { rows: DnsRR[]; title: string }) {
  if (!rows.length) return null;
  return (
    <>
      <p className="k-small k-muted">;; {title} SECTION:</p>
      <pre className="k-pre">{rows.map((r) => `${r.name}\t${r.ttl}\t${r.class ?? 'IN'}\t${r.type}\t${r.data}`).join('\n')}</pre>
    </>
  );
}

function Dig() {
  const api = useApi();
  const typeId = useId();
  const [name, setName] = useState('');
  const [type, setType] = useState('A');
  const [client, setClient] = useState('');
  const [err, setErr] = useState<string>();
  const m = useMutation({ mutationFn: () => api.runtime.dig({ name: name.trim(), type, ...(client.trim() ? { client: client.trim() } : {}) }) });
  const r: DnsQueryResult | undefined = m.data;
  return (
    <section className="k-panel" aria-labelledby="dig-title">
      <h2 id="dig-title" className="k-h3">
        Query (dig)
      </h2>
      <p className="k-muted k-small">Runs the query through the full pipeline (policy, blocking, zones, cache, upstreams) as the given client, without touching the network listener.</p>
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
        <TextInput label="As client" value={client} onChange={setClient} placeholder="192.168.1.30" isOptional />
        <Button type="submit" label="Query" variant="primary" isLoading={m.isPending} />
      </form>
      {m.error ? <ErrorBanner error={m.error} /> : null}
      {r ? (
        <div className="k-stack" aria-live="polite">
          <p className="k-row">
            <Badge variant={r.status_class === 'blocked' ? 'error' : r.status_class === 'error' ? 'warning' : 'success'} label={r.status.replace(/_/g, ' ')} />
            <span className="k-mono">{r.rcode}</span>
            <span className="k-muted k-small">{formatMS(r.took_ms)}</span>
            {r.node ? <span className="k-muted k-small">node {r.node}</span> : null}
          </p>
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
            {r.zone ? (
              <div>
                <dt>Zone</dt>
                <dd>{r.zone}</dd>
              </div>
            ) : null}
            {r.cname_chain?.length ? (
              <div>
                <dt>CNAME chain</dt>
                <dd className="k-mono">{r.cname_chain.join(' → ')}</dd>
              </div>
            ) : null}
            {r.ede ? (
              <div>
                <dt>Extended DNS error</dt>
                <dd>
                  {edeText(r.ede.code)}
                  {r.ede.text ? `: ${r.ede.text}` : ''}
                </dd>
              </div>
            ) : null}
          </dl>
          <Section title="ANSWER" rows={r.answer} />
          <Section title="AUTHORITY" rows={r.authority} />
          <Section title="ADDITIONAL" rows={r.additional} />
          {!r.answer.length && !r.authority.length ? <p className="k-muted">No records in the answer.</p> : null}
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
      await settle('version', api.cluster.version(), (v) => [{ ok: true, text: `kudurru ${v.version} (${v.commit}, ${v.go ?? 'go'})` }]);
      await settle('nodes', api.cluster.nodes(), (n) => n.items.map((x) => ({ ok: x.ready, text: `node ${x.name}: ${x.ready ? 'ready' : 'NOT READY'}${x.leader ? ', leader' : ''}, ${x.version}` })));
      const nodes = await api.cluster.nodes().catch(() => ({ items: [] }));
      for (const n of nodes.items) {
        await settle(`health ${n.name}`, api.cluster.health(n.name), (h) => h.checks.map((c) => ({ ok: c.ok, text: `  ${n.name} ${c.name}: ${c.ok ? 'ok' : 'FAIL'}${c.message ? ` (${c.message})` : ''}` })));
      }
      await settle('query log', api.queries.status(), (s) => [
        { ok: s.wal_bytes < 256 * 1024 * 1024 ? true : 'warn', text: `query log: ${formatExact(s.rows)} rows, db ${formatBytes(s.db_bytes)}, WAL ${formatBytes(s.wal_bytes)}, oldest ${formatDateTime(s.oldest_ts)}` },
      ]);
      await settle('diagnosis', api.cluster.messages(), (msgs) =>
        msgs.items.length ? msgs.items.map((x) => ({ ok: x.severity === 'error' ? false : 'warn', text: `${x.severity}: ${x.reason}: ${x.message}` })) : [{ ok: true, text: 'no diagnosis messages' }],
      );
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
