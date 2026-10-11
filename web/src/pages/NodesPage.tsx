// Nodes (§10 principles 9 and 13): version, roles, ready, QPS, leader, VIP
// master and snapshot age per node (GET /nodes, scatter-gathered by the
// server), the cluster summary, the edge (dnsdist backends, VIP owners)
// and the health checks of one node on demand.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../api/context';
import { useNodes } from '../api/hooks';
import { ErrorBanner, Loading } from '../components/Feedback';
import { formatQPS, formatRelative } from '../lib/format';

function Health({ name }: { name: string }) {
  const api = useApi();
  const q = useQuery({ queryKey: ['nodes', name, 'health'], queryFn: () => api.cluster.health(name) });
  if (q.error) return <ErrorBanner error={q.error} />;
  if (!q.data) return <Loading label={`Checking ${name}`} />;
  return (
    <ul className="k-checks">
      {q.data.checks.map((c) => (
        <li key={c.name}>
          <StatusDot variant={c.ok ? 'success' : 'error'} label={c.ok ? 'ok' : 'failing'} /> <strong>{c.name}</strong>
          {c.message ? <span className="k-muted"> {c.message}</span> : null}
        </li>
      ))}
    </ul>
  );
}

export default function NodesPage() {
  const api = useApi();
  const nodes = useNodes();
  const cluster = useQuery({ queryKey: qk.cluster, queryFn: () => api.cluster.info() });
  const edge = useQuery({ queryKey: ['edge'], queryFn: () => api.cluster.edge() });
  const [open, setOpen] = useState<string | null>(null);
  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Nodes</h1>
        {cluster.data ? (
          <span className="k-row k-small">
            <Badge variant="neutral" label={cluster.data.mode} />
            <span>leader {cluster.data.leader || 'none'}</span>
            {cluster.data.readOnly ? <Badge variant="warning" label="read-only" /> : null}
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
                <th scope="col">Version</th>
                <th scope="col">Roles</th>
                <th scope="col" className="num">
                  Queries/s
                </th>
                <th scope="col">Leader</th>
                <th scope="col">VIP</th>
                <th scope="col">Snapshot age</th>
                <th scope="col">Store</th>
                <th scope="col">
                  <span className="k-visually-hidden">Health</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {nodes.data.items.map((n) => (
                <tr key={n.name}>
                  <td>
                    <strong>{n.name}</strong>
                    {n.address ? <div className="k-muted k-small k-mono">{n.address}</div> : null}
                    {n.startedAt ? <div className="k-muted k-small">up since {formatRelative(n.startedAt)}</div> : null}
                  </td>
                  <td>
                    <StatusDot variant={n.ready ? 'success' : 'error'} label={n.ready ? 'ready' : 'not ready'} /> {n.ready ? 'ready' : 'not ready'}
                  </td>
                  <td className="k-mono k-small">{n.version}</td>
                  <td className="k-small">{n.roles.join(', ')}</td>
                  <td className="num">{formatQPS(n.qps)}</td>
                  <td>{n.leader ? <Badge variant="blue" label="leader" /> : <span className="k-muted">follower</span>}</td>
                  <td>{n.vipMaster ? <Badge variant="purple" label="VIP master" /> : <span className="k-muted">–</span>}</td>
                  <td>{n.snapshotAge ?? '–'}</td>
                  <td className="k-small">{n.storeDriver ?? '–'}</td>
                  <td>
                    <Button label={open === n.name ? 'Hide health' : 'Health'} size="sm" variant="ghost" onClick={() => setOpen(open === n.name ? null : n.name)} />
                  </td>
                </tr>
              ))}
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
      <section className="k-panel" aria-labelledby="edge-title">
        <h2 id="edge-title" className="k-h3">
          Edge
        </h2>
        {edge.error ? <ErrorBanner error={edge.error} /> : null}
        {edge.data && edge.data.backends.length === 0 && !edge.data.vip?.length ? (
          <p className="k-muted">No dnsdist edge or VIP in this deployment.</p>
        ) : null}
        {edge.data?.backends.length ? (
          <ul>
            {edge.data.backends.map((b) => (
              <li key={b.name}>
                <StatusDot variant={b.up ? 'success' : 'error'} label={b.up ? 'up' : 'down'} /> {b.name} <span className="k-mono k-muted">{b.address}</span>
                {b.qps !== undefined ? ` ${formatQPS(b.qps)} q/s` : ''}
              </li>
            ))}
          </ul>
        ) : null}
        {edge.data?.vip?.length ? (
          <ul>
            {edge.data.vip.map((v) => (
              <li key={v.address}>
                VIP <span className="k-mono">{v.address}</span> on {v.owner ?? 'nobody'}
              </li>
            ))}
          </ul>
        ) : null}
      </section>
    </div>
  );
}
