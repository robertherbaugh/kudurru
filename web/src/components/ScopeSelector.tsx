// "All nodes" or one node (§10 principle 9). Every stats and query-log
// request carries the choice as scope=cluster|node&node=<name>; with one
// node both give the same answer, but the control and the parameter are
// there from v0.1 so multi-node needs no UI change.
import { useScope } from '../api/context';
import { useNodes } from '../api/hooks';
import { SelectField } from './fields/SelectField';

export function ScopeSelector() {
  const { scope, node, setScope } = useScope();
  const nodes = useNodes();
  const value = scope === 'node' && node ? node : '';
  const names = nodes.data?.items.map((n) => n.name) ?? [];
  // A chosen node that is no longer listed stays (SelectField keeps unknown values).
  return (
    <SelectField
      label="Node scope"
      isLabelHidden
      size="sm"
      value={value}
      emptyLabel={`All nodes${names.length ? ` (${names.length})` : ''}`}
      options={names.map((n) => ({ value: n, label: n }))}
      onChange={(v) => setScope(v ? { scope: 'node', node: v } : { scope: 'cluster' })}
    />
  );
}

/** "Showing 2 nodes" / "node kudurru-0" with partial-result warning. */
export function ScopeNote({ scope }: { scope?: { scope: string; nodes: { name: string; ok: boolean; error?: string }[]; partial: boolean } }) {
  if (!scope) return null;
  const failed = scope.nodes.filter((n) => !n.ok);
  return (
    <span className="k-small k-muted" data-testid="scope-note">
      {scope.scope === 'node' ? `Node ${scope.nodes.map((n) => n.name).join(', ')}` : `All nodes: ${scope.nodes.map((n) => n.name).join(', ')}`}
      {scope.partial ? ` (partial: ${failed.map((n) => n.name).join(', ')} did not answer)` : ''}
    </span>
  );
}
