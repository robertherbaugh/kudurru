// The sidebar status block (§10 principle 1): queries per second, an
// overall status dot, the blocking switch with its countdown and one dot
// per node. One GET /stats/summary (shared with the dashboard tiles) and
// GET /nodes feed it.
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { useSideNavCollapse } from '@astryxdesign/core/SideNav';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Text } from '@astryxdesign/core/Text';
import { useLicense, useNodes, useSummary } from '../api/hooks';
import { formatQPS, formatVersion } from '../lib/format';
import { navPlanLabel } from '../lib/license';
import { readiness } from '../lib/nodes';
import { BlockingControl } from './BlockingControl';

export function SidebarStatus() {
  const summary = useSummary();
  const nodes = useNodes();
  const license = useLicense();
  const { isCollapsed } = useSideNavCollapse();
  const list = nodes.data?.items ?? [];
  // Use this server's version, regardless of the selected stats node.
  const version = formatVersion(list.find((n) => n.self)?.version);
  const plan = navPlanLabel(license.isError ? undefined : license.data);
  // Peers report no readiness yet; only an explicit false counts as down.
  const down = list.filter((n) => n.ready === false);
  const healthy = !summary.isError && !nodes.isError && down.length === 0;
  const statusLabel = summary.isError
    ? 'API unreachable'
    : down.length
      ? `${down.length} node${down.length === 1 ? '' : 's'} not ready`
      : 'All systems normal';
  return (
    <VStack as="section" aria-label="Status" gap={1.5} paddingBlock={2} paddingInline={3}>
      <HStack gap={2} vAlign="center">
        <StatusDot variant={healthy ? 'success' : summary.isError ? 'error' : 'warning'} label={statusLabel} isPulsing={!healthy} />
        <Text type="supporting" color="primary">
          {statusLabel}
        </Text>
      </HStack>
      <HStack gap={2} hAlign="between" vAlign="center">
        <Text type="supporting">Queries/s</Text>
        <Text type="code" size="sm" weight="bold" data-testid="sidebar-qps">
          {formatQPS(summary.data?.queries.qps_1m)}
        </Text>
      </HStack>
      <BlockingControl compact />
      {list.length > 0 ? (
        <HStack gap={2} hAlign="between" vAlign="center">
          <Text type="supporting">Nodes</Text>
          <HStack gap={1.5} vAlign="center" wrap="wrap">
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
          </HStack>
        </HStack>
      ) : null}
      {!isCollapsed ? (
        <HStack role="group" aria-label="Running version and license" gap={2} hAlign="between" vAlign="center" wrap="wrap">
          <Text type="supporting" maxLines={1}>{version}</Text>
          {plan ? <Text type="supporting" maxLines={1}>{plan}</Text> : null}
        </HStack>
      ) : null}
    </VStack>
  );
}
