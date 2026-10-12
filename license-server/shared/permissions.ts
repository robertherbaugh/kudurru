// Who may do what in the license portal (design spec §5): the three roles,
// the permission vocabulary and the fixed role → permission table. The
// server authorises every route by one permission from shared/routes.ts;
// the UI shows navigation and actions by the same permissions (Me.permissions).
// Roles are not hierarchical in storage, but superadmin holds everything
// license_admin holds, and both hold everything customer holds.

/** A role. Effective roles = users.idp_roles ∪ users.granted_roles; a disabled user has none. */
export type Role = 'customer' | 'license_admin' | 'superadmin';

/** Every role, from least to most powerful. */
export const ROLES: readonly Role[] = ['customer', 'license_admin', 'superadmin'];

/**
 * A permission. `self.*` covers the signed-in person's own memberships,
 * licenses and requests (customer isolation is enforced in SQL by
 * membership); the rest are the licensor's back office.
 */
export type Permission =
  /** Own memberships, the licenses and current keys of member customers, own requests. */
  | 'self.read'
  /** Request a personal key, renew an own personal key, redeem an invite. */
  | 'self.request'
  | 'customers.read'
  | 'customers.write'
  | 'licenses.read'
  /** Issue, import and email keys. */
  | 'licenses.issue'
  | 'licenses.renew'
  | 'licenses.change'
  | 'licenses.withdraw'
  | 'requests.read'
  | 'requests.decide'
  | 'blocklist.read'
  /** Remove blocked emails. */
  | 'blocklist.write'
  | 'audit.read'
  | 'users.read'
  /** Granted roles, disable and enable, revoke sessions. */
  | 'users.write'
  /** The System page. */
  | 'system.read';

/** Every permission, in the order of spec §5 (Me.permissions is sorted this way). */
export const PERMISSIONS: readonly Permission[] = [
  'self.read',
  'self.request',
  'customers.read',
  'customers.write',
  'licenses.read',
  'licenses.issue',
  'licenses.renew',
  'licenses.change',
  'licenses.withdraw',
  'requests.read',
  'requests.decide',
  'blocklist.read',
  'blocklist.write',
  'audit.read',
  'users.read',
  'users.write',
  'system.read',
];

const CUSTOMER: readonly Permission[] = ['self.read', 'self.request'];

const LICENSE_ADMIN: readonly Permission[] = [
  ...CUSTOMER,
  'customers.read',
  'customers.write',
  'licenses.read',
  'licenses.issue',
  'licenses.renew',
  'licenses.change',
  'licenses.withdraw',
  'requests.read',
  'requests.decide',
  'blocklist.read',
  'audit.read',
  'users.read',
  'system.read',
];

const SUPERADMIN: readonly Permission[] = [...LICENSE_ADMIN, 'blocklist.write', 'users.write'];

/** The fixed table of spec §5. */
export const ROLE_PERMISSIONS: Readonly<Record<Role, readonly Permission[]>> = Object.freeze({
  customer: CUSTOMER,
  license_admin: LICENSE_ADMIN,
  superadmin: SUPERADMIN,
});

export interface RoleInfo {
  key: Role;
  label: string;
  /** One sentence for the Users page. */
  description: string;
}

/** The roles as people read them, in the order of ROLES. */
export const ROLE_INFO: readonly RoleInfo[] = [
  { key: 'customer', label: 'Customer', description: 'Sees the licenses and keys of the customers they belong to, and requests a free personal key.' },
  { key: 'license_admin', label: 'License admin', description: 'Manages customers, issues, renews, changes and withdraws licenses, and decides requests.' },
  { key: 'superadmin', label: 'Superadmin', description: 'Everything a license admin does, plus user roles, sessions and the blocklist.' },
];

export function isRole(v: unknown): v is Role {
  return typeof v === 'string' && (ROLES as readonly string[]).includes(v);
}

export function isPermission(v: unknown): v is Permission {
  return typeof v === 'string' && (PERMISSIONS as readonly string[]).includes(v);
}

export function roleLabel(role: string): string {
  return ROLE_INFO.find((r) => r.key === role)?.label ?? role;
}

/**
 * The effective roles of a user: the union of the roles the IdP maps and
 * the roles granted in the portal, in ROLES order; none while disabled.
 * Unknown strings are dropped.
 */
export function effectiveRoles(idpRoles: readonly string[], grantedRoles: readonly string[], disabled = false): Role[] {
  if (disabled) return [];
  const held = new Set<string>([...idpRoles, ...grantedRoles]);
  return ROLES.filter((r) => held.has(r));
}

/** The permissions the roles grant together, in PERMISSIONS order. */
export function permissionsFor(roles: Iterable<Role>): Permission[] {
  const held = new Set<Permission>();
  for (const role of roles) for (const p of ROLE_PERMISSIONS[role]) held.add(p);
  return PERMISSIONS.filter((p) => held.has(p));
}

/** Whether a set of held permissions (Me.permissions, Principal.permissions) includes `needed`. */
export function hasPermission(held: readonly Permission[] | ReadonlySet<Permission>, needed: Permission): boolean {
  return Array.isArray(held) ? held.includes(needed) : (held as ReadonlySet<Permission>).has(needed);
}
