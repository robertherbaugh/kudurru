// The route manifest and the role table (design spec §5, §8): every route
// is unique and well-formed, buildPath encodes parameters, and the roles
// nest (superadmin ⊇ license_admin ⊇ customer) with exactly the
// differences the spec lists.
import { describe, expect, test } from 'bun:test';
import { ROUTE_TYPES_COMPLETE } from '../../shared/api';
import { isPermission, permissionsFor, ROLE_PERMISSIONS, effectiveRoles, hasPermission } from '../../shared/permissions';
import { API_PREFIX, buildPath, ROUTE_LIST } from '../../shared/routes';

describe('ROUTES', () => {
  test('one route per method and path, every path under /api/v1 or a health path', () => {
    const keys = ROUTE_LIST.map((r) => `${r.method} ${r.path}`);
    expect(new Set(keys).size).toBe(keys.length);
    for (const r of ROUTE_LIST) {
      expect(r.path === '/healthz' || r.path === '/readyz' || r.path.startsWith(`${API_PREFIX}/`)).toBe(true);
      expect(['anonymous', 'public', 'session'].includes(r.access) || isPermission(r.access)).toBe(true);
    }
    expect(ROUTE_TYPES_COMPLETE).toBe(true);
  });

  test('only the kudurru.io form is public, with CORS and a preflight', () => {
    expect(ROUTE_LIST.filter((r) => r.access === 'public').map((r) => r.id)).toEqual(['publicKeyRequest']);
    expect(ROUTE_LIST.filter((r) => r.cors || r.preflight || r.badBody).map((r) => r.id)).toEqual(['publicKeyRequest']);
  });

  test('buildPath', () => {
    expect(buildPath('licenseGet', { id: 'lic_x' })).toBe('/api/v1/licenses/lic_x');
    expect(buildPath('memberRemove', { id: 'cus_x', email: 'a+b@c.org' })).toBe('/api/v1/customers/cus_x/members/a%2Bb%40c.org');
    expect(buildPath('licenseKeyDownload', { id: 'lic_x', keyId: 'key_y' })).toBe('/api/v1/licenses/lic_x/keys/key_y/download');
    expect(buildPath('licensesList', {}, { plan: 'personal', q: undefined, limit: 20 })).toBe('/api/v1/licenses?plan=personal&limit=20');
    expect(buildPath('authMe')).toBe('/api/v1/auth/me');
    expect(() => buildPath('licenseGet', { id: '' })).toThrow('missing parameter id');
  });
});

describe('roles', () => {
  test('superadmin ⊇ license_admin ⊇ customer', () => {
    for (const p of ROLE_PERMISSIONS.customer) expect(ROLE_PERMISSIONS.license_admin).toContain(p);
    for (const p of ROLE_PERMISSIONS.license_admin) expect(ROLE_PERMISSIONS.superadmin).toContain(p);
    expect(ROLE_PERMISSIONS.customer).toEqual(['self.read', 'self.request']);
    expect(ROLE_PERMISSIONS.superadmin.filter((p) => !ROLE_PERMISSIONS.license_admin.includes(p))).toEqual(['blocklist.write', 'users.write']);
  });

  test('effective roles are the union, none while disabled', () => {
    expect(effectiveRoles(['customer'], ['superadmin'])).toEqual(['customer', 'superadmin']);
    expect(effectiveRoles(['license_admin', 'bogus'], [])).toEqual(['license_admin']);
    expect(effectiveRoles(['superadmin'], ['superadmin'], true)).toEqual([]);
    const perms = permissionsFor(['customer', 'license_admin']);
    expect(hasPermission(perms, 'licenses.issue')).toBe(true);
    expect(hasPermission(new Set(perms), 'users.write')).toBe(false);
  });
});
