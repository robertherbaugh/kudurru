// Roles and scopes of internal/rbac as the dashboard offers them: the
// built-in roles, and the scopes a role binding or an API token can name
// (cluster, one zone, or a zone group: every zone labeled
// kudurru.io/zonegroup with that group, D385).
import type { EnumOption } from '../components/fields/EnumSelect';

/** The roles of internal/rbac (the rbac.bindings role enum of the settings schema). */
export const ROLES: EnumOption[] = [
  { value: 'viewer', label: 'Viewer', help: 'Read zones, records, lists, policies, stats and settings.' },
  { value: 'auditor', label: 'Auditor', help: 'Viewer plus the query log and the audit log.' },
  { value: 'operator', label: 'Operator', help: 'Manage records, lists, policies, groups, clients and blocking; flush the cache.' },
  { value: 'zone-admin', label: 'Zone admin', help: 'Manage zones and their records, transfers and signing.' },
  { value: 'admin', label: 'Admin', help: 'Everything.' },
];

export type ScopeKind = 'cluster' | 'zone' | 'zonegroup';

export const SCOPE_KINDS: EnumOption[] = [
  { value: 'cluster', label: 'Everything', help: 'The role applies to every object.' },
  { value: 'zone', label: 'One zone', help: 'The role applies to one zone and its records.' },
  {
    value: 'zonegroup',
    label: 'A zone group',
    help: 'The role applies to every zone labeled kudurru.io/zonegroup with this group, and their records. Removing the label takes the access away.',
  },
];

/** The scope string of a token restricted to a role: "viewer", "zone-admin@zone:home-arpa" or "zone-admin@zonegroup:prod". */
export function scopeString(role: string, kind: string, name: string): string {
  if (!role) return '';
  const n = name.trim();
  if ((kind === 'zone' || kind === 'zonegroup') && n) return `${role}@${kind}:${n}`;
  return role;
}
