// The sidebar status block (§10 principle 1): queries per second, an
// overall status dot, the blocking switch with its countdown and one dot
// per node. One GET /stats/summary (shared with the dashboard tiles) and
// GET /nodes feed it.
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useNodes, useSummary } from '../api/hooks';
import { formatQPS } from '../lib/format';
import { readiness } from '../lib/nodes';
import { BlockingControl } from './BlockingControl';

export function SidebarStatus() {
  const summary = useSummary();
  const nodes = useNodes();
  const list = nodes.data?.items ?? [];
  // Peers report no readiness yet; only an explicit false counts as down.
  const down = list.filter((n) => n.ready === false);
  const healthy = !summary.isError && !nodes.isError && down.length === 0;
  const statusLabel = summary.isError
    ? 'API unreachable'
    : down.length
      ? `${down.length} node${down.length === 1 ? '' : 's'} not ready`
      : 'All systems normal';
  return (
    <section className="k-status" aria-label="Status">
      <div className="k-status-row">
        <span className="k-row">
          <StatusDot variant={healthy ? 'success' : summary.isError ? 'error' : 'warning'} label={statusLabel} isPulsing={!healthy} />
          <span>{statusLabel}</span>
        </span>
      </div>
      <div className="k-status-row">
        <span className="k-muted">Queries/s</span>
        <strong className="k-mono" data-testid="sidebar-qps">
          {formatQPS(summary.data?.queries.qps_1m)}
        </strong>
      </div>
      <BlockingControl compact />
      {list.length > 0 ? (
        <div className="k-status-row">
          <span className="k-muted">Nodes</span>
          <span className="k-node-dots">
            {list.map((n) => {
              const r = readiness(n);
              return (
                <StatusDot
                  key={n.name}
                  variant={r.variant}
                  label={`${n.name}: ${r.label}${n.leader ? ', leader' : ''}`}
                  tooltip={`${n.name}${n.leader ? ' (leader)' : ''}: ${r.label}${n.state ? `, zones ${n.state}` : ''}`}
                />
              );
            })}
          </span>
        </div>
      ) : null}
    </section>
  );
}
