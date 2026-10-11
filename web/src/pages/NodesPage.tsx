// Nodes (§10 principles 9 and 13): readiness, zonecache state, version,
// leader and snapshot age per node (GET /nodes), the cluster summary
// (GET /cluster: store driver, epoch, leader), the edge supervisors
// (GET /edge: dnsdist and keepalived state, dnsdist backends) and the
// health components of this node on demand (GET /nodes/{name}/health).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../api/context';
import { useNodes } from '../api/hooks';
import type { EdgeComponentState } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { formatRelative } from '../lib/format';
import { readiness } from '../lib/nodes';

function Health({ name }: { name: string }) {
  const api = useApi();
  const q = useQuery({ queryKey: ['nodes', name, 'health'], queryFn: () => api.cluster.health(name) });
  if (q.error) return <ErrorBanner error={q.error} />;
  if (!q.data) return <Loading label={`Checking ${name}`} />;
  const h = q.data.health;
  const components = Object.entries(h.components ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className="k-stack">
      <p className="k-row">
        <StatusDot variant={q.data.ready ? 'success' : 'error'} label={q.data.ready ? 'ready' : 'not ready'} />
        <strong>{h.status ?? (q.data.ready ? 'ready' : 'not ready')}</strong>
        {h.version ? <span className="k-muted k-small">version {h.version}</span> : null}
      </p>
      {h.unready?.length ? <p className="k-small">Waiting for: {h.unready.join(', ')}</p> : null}
      {components.length ? (
        <table className="k-table" aria-label={`Health components of ${name}`}>
          <thead>
            <tr>
              <th scope="col">Component</th>
              <th scope="col">State</th>
              <th scope="col">Since</th>
            </tr>
          </thead>
          <tbody>
            {components.map(([c, v]) => (
              <tr key={c}>
                <td>
                  <strong>{c}</strong>
                </td>
                <td>
                  <StatusDot variant={v.ready ? 'success' : 'error'} label={v.ready ? 'ready' : 'not ready'} /> {v.ready ? 'ready' : 'not ready'}
                  {v.message ? <span className="k-muted"> {v.message}</span> : null}
                </td>
                <td className="k-small">{v.since ? <span title={v.since}>{formatRelative(v.since)}</span> : '–'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="k-muted">The node reported no components.</p>
      )}
    </div>
  );
}

function EdgeRow({ label, c, off }: { label: string; c: EdgeComponentState | undefined; off: string }) {
  if (!c) return null;
  return (
    <div>
      <dt>{label}</dt>
      <dd>
        {c.enabled ? (
          <>
            <StatusDot variant={c.state && /fail|error|down|fault/i.test(c.state) ? 'error' : 'success'} label={c.state ?? 'enabled'} /> {c.state ?? 'enabled'}
          </>
        ) : (
          <span className="k-muted">{off}</span>
        )}
      </dd>
    </div>
  );
}

export default function NodesPage() {
  const api = useApi();
  const nodes = useNodes();
  const cluster = useQuery({ queryKey: qk.cluster, queryFn: () => api.cluster.info() });
  const edge = useQuery({ queryKey: ['edge'], queryFn: () => api.cluster.edge() });
  const [open, setOpen] = useState<string | null>(null);
  const c = cluster.data;
  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Nodes</h1>
        {c ? (
          <span className="k-row k-small">
            <Badge variant="neutral" label={`store ${c.driver}`} />
            <span>leader {c.leader || 'none'}</span>
            {c.readOnly ? <Badge variant="warning" label="read-only" /> : null}
          </span>
        ) : null}
      </div>
      {nodes.error ? <ErrorBanner error={nodes.error} /> : null}
      {nodes.isLoading ? <Loading /> : null}
      {nodes.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Nodes">
            <thead>
              <tr>
                <th scope="col">Node</th>
                <th scope="col">Ready</th>
                <th scope="col">Zones</th>
                <th scope="col">Version</th>
                <th scope="col">Role</th>
                <th scope="col">Snapshot age</th>
                <th scope="col">
                  <span className="k-visually-hidden">Health</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {nodes.data.items.map((n) => {
                const r = readiness(n);
                return (
                  <tr key={n.name}>
                    <td>
                      <strong>{n.name}</strong> {n.self ? <Badge variant="neutral" label="this node" /> : null}
                      {n.address ? <div className="k-muted k-small k-mono">{n.address}</div> : null}
                    </td>
                    <td>
                      <StatusDot variant={r.variant} label={r.label} /> {r.label}
                      {n.unready?.length ? <div className="k-muted k-small">waiting for {n.unready.join(', ')}</div> : null}
                    </td>
                    <td className="k-small">{n.state ?? '–'}</td>
                    <td className="k-mono k-small">{n.version ?? '–'}</td>
                    <td>{n.leader ? <Badge variant="blue" label="leader" /> : <span className="k-muted">follower</span>}</td>
                    <td>{n.snapshotAge ?? '–'}</td>
                    <td>
                      <Button
                        label={open === n.name ? 'Hide health' : 'Health'}
                        size="sm"
                        variant="ghost"
                        isDisabled={!n.self}
                        tooltip={n.self ? undefined : `Ask ${n.name} directly; other nodes' health needs scatter-gather`}
                        onClick={() => setOpen(open === n.name ? null : n.name)}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}
      {open ? (
        <section className="k-panel" aria-labelledby="health-title">
          <h2 id="health-title" className="k-h3">
            Health of {open}
          </h2>
          <Health name={open} />
        </section>
      ) : null}
      <div className="k-grid k-grid-2">
        <section className="k-panel" aria-labelledby="cluster-title">
          <h2 id="cluster-title" className="k-h3">
            Cluster
          </h2>
          {cluster.error ? <ErrorBanner error={cluster.error} /> : null}
          {c ? (
            <dl className="k-why-grid">
              <div>
                <dt>Store</dt>
                <dd>{c.driver}</dd>
              </div>
              <div>
                <dt>Leader</dt>
                <dd>{c.leader || 'none'}</dd>
              </div>
              <div>
                <dt>Nodes</dt>
                <dd>{c.nodes}</dd>
              </div>
              <div>
                <dt>Epoch</dt>
                <dd className="k-mono">{c.epoch}</dd>
              </div>
              <div>
                <dt>Object writes</dt>
                <dd>{c.readOnly ? 'refused (objects are file-managed on every node)' : 'accepted'}</dd>
              </div>
              {c.clusterID ? (
                <div>
                  <dt>Cluster ID</dt>
                  <dd className="k-mono k-small">{c.clusterID}</dd>
                </div>
              ) : null}
            </dl>
          ) : null}
        </section>
        <section className="k-panel" aria-labelledby="edge-title">
          <h2 id="edge-title" className="k-h3">
            Edge
          </h2>
          {edge.error ? <ErrorBanner error={edge.error} /> : null}
          {edge.data ? (
            <>
              <dl className="k-why-grid">
                <EdgeRow label="dnsdist" c={edge.data.edge} off="not running on this node" />
                <EdgeRow label="VIP (keepalived)" c={edge.data.vip} off="not running on this node" />
              </dl>
              {edge.data.backends.length ? (
                <ul>
                  {edge.data.backends.map((b) => (
                    <li key={b.name}>
                      <StatusDot variant={b.up ? 'success' : 'error'} label={b.up ? 'up' : 'down'} /> {b.name} <span className="k-mono k-muted">{b.address}</span>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="k-muted k-small">No dnsdist backends reported.</p>
              )}
            </>
          ) : null}
        </section>
      </div>
    </div>
  );
}
