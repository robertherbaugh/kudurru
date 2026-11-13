// The client inventory view (GET /inventory, D397): what each address state
// means in words, which Integration or DHCP scope reported a device, and
// when to say that DHCP leases are this node's own.
import { ApiError } from '../api/client';
import type { ClusterInfo, InventoryAddressState, InventoryDevice, IntegrationSummary } from '../api/types';
import type { ClusterRole } from './follower';

/** Each address state, as the inventory API defines it (internal/api/inventory.go). */
export const ADDRESS_STATE_TEXT: Record<InventoryAddressState, { label: string; help: string }> = {
  assigned: { label: 'Assigned', help: 'Queries from this address get this device’s identity.' },
  expired: { label: 'Expired', help: 'The observation or lease passed its expiry; the address no longer identifies this device.' },
  ambiguous: {
    label: 'Ambiguous',
    help: 'An equally new claim of another device collides (for DHCP: another scope leases the address to a different MAC), so neither device identifies it.',
  },
  superseded: { label: 'Superseded', help: 'A newer claim of another device owns the address.' },
  withdrawn: { label: 'Withdrawn', help: 'The device’s own source later removed this claim.' },
};

/** The device's display name: its name, else its hostname, else its ID. */
export function deviceName(d: InventoryDevice): string {
  return d.name || d.hostname || d.id;
}

/** The DHCP scope of a "dhcp/<scope>" source, undefined for any other. */
export function dhcpScope(source: string): string | undefined {
  return source.startsWith('dhcp/') ? source.slice('dhcp/'.length) : undefined;
}

export interface SourceRef {
  /** Words for the source: "Integration office", "DHCP scope lan" or the raw ID. */
  label: string;
  /** The page of the Integration or DHCP scope, when one is known. */
  href?: string;
}

/**
 * The page of the source that reported a device: the Integration whose
 * incarnation carries this inventory source ID (GET /integrations
 * inventorySource), or the DHCP scope of a lease. A source whose Integration
 * is not known (deleted, or the session cannot read integrations) keeps its ID.
 */
export function sourceRef(source: string, integrations: ReadonlyMap<string, IntegrationSummary>, integrationHref: (ns: string, name: string) => string): SourceRef {
  const scope = dhcpScope(source);
  if (scope !== undefined) return { label: `DHCP scope ${scope}`, href: `/dhcp/${encodeURIComponent(scope)}` };
  const owner = integrations.get(source);
  if (owner) return { label: `Integration ${owner.namespace === 'default' ? owner.name : `${owner.namespace}/${owner.name}`}`, href: integrationHref(owner.namespace, owner.name) };
  return { label: source };
}

/** The Integration summaries keyed by the inventory source their devices carry. */
export function integrationsBySource(summaries: Iterable<IntegrationSummary>): Map<string, IntegrationSummary> {
  const out = new Map<string, IntegrationSummary>();
  for (const s of summaries) if (s.inventorySource) out.set(s.inventorySource, s);
  return out;
}

/**
 * Whether the answering node is one member of a SQLite cluster, where DHCP
 * lease MACs and lease devices are node-local (D397): GET /cluster says so
 * (SQLite with more than one node, or a primary or follower role), or, for a
 * session without cluster.read, the public status names a follower.
 */
export function sqliteClusterMember(info: ClusterInfo | undefined, role: ClusterRole): boolean {
  if (info) return info.driver === 'sqlite' && (info.nodes > 1 || info.role === 'primary' || info.role === 'follower');
  return role.follower;
}

/** What a 503 of GET /inventory means; undefined for every other error. */
export function inventoryUnavailable(err: unknown): 'unmounted' | 'unavailable' | undefined {
  if (!(err instanceof ApiError) || err.status !== 503 || err.key !== 'unavailable') return undefined;
  return /not mounted/.test(err.message) ? 'unmounted' : 'unavailable';
}
