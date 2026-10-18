// "All nodes" or one node (§10 principle 9). Every stats and query-log
// request carries the choice as scope=cluster|node&node=<name>; with one
// node both give the same answer, but the control and the parameter are
// there from v0.1 so multi-node needs no UI change.
import { Text } from '@astryxdesign/core/Text';
import { useScope } from '../api/context';
import { useNodes } from '../api/hooks';
import { SelectField } from './fields/SelectField';

export function ScopeSelector({ allLabel, response }: { allLabel?: string; response?: ResponseScope }) {
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
      emptyLabel={allLabel ?? `All nodes${names.length ? ` (${names.length})` : ''}`}
      options={names.map((n) => ({ value: n, label: n }))}
      help={scopeWarning(response)}
      onChange={(v) => setScope(v ? { scope: 'node', node: v } : { scope: 'cluster' })}
    />
  );
}

export interface ResponseScope {
  scope: string;
  nodes: { name: string; ok: boolean; error?: string }[];
  partial: boolean;
  /** "central" when the central database answered (D161). */
  source?: string;
}

function scopeWarning(scope?: ResponseScope): string | undefined {
  if (!scope?.partial) return undefined;
  const failed = scope.nodes.filter((n) => !n.ok).length;
  return failed ? `Partial results: ${failed} ${failed === 1 ? 'node' : 'nodes'} delayed or unavailable` : 'Partial results';
}

/** A bounded summary: node counts instead of a growing list of pod names. */
export function ScopeNote({ scope }: { scope?: ResponseScope }) {
  if (!scope) return null;
  return (
    <Text type="supporting" data-testid="scope-note">
      {scope.scope === 'node' ? `Node ${scope.nodes[0]?.name ?? ''}` : `All nodes (${scope.nodes.length})`}
      {scope.source === 'central' ? ' (central database)' : ''}
      {scope.partial ? ` · ${scopeWarning(scope)}` : ''}
    </Text>
  );
}
