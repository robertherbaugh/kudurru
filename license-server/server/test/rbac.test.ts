// The RBAC matrix (design spec §5, §10): every route of shared/routes.ts ×
// {anonymous, customer, license_admin, superadmin}, through the production
// server with the real pipeline and the merged handler map of
// server/api/index.ts. Only the authorization layer is asserted, so the
// matrix holds whatever the domain handlers answer behind it (404 for the
// made-up ids, 400 for the empty bodies, 501 for a route without a
// handler yet, 503 without a signer or IdP are all fine):
// - no session on a session or permission route → 401 unauthenticated;
// - a role without the route's permission → 403 forbidden, hint {missing};
// - a role with it (and anyone on anonymous and public routes) → neither.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import type { ApiErrorBody, MissingPermissionHint } from '../../shared/api';
import { ROLE_PERMISSIONS, type Permission, type Role } from '../../shared/permissions';
import { ROUTE_LIST, type RouteDef } from '../../shared/routes';
import { handlers } from '../api';
import type { Config } from '../config';
import { createPipeline } from '../http/pipeline';
import { MemoryLogger } from '../log';
import { call, makeTestApp, type TestApp } from './call';
import { describeDb, useTestDb } from './db';
import { createUser, signInAs, testConfig, type TestUser } from './fixtures';

const ROLES_UNDER_TEST: readonly (Role | 'anonymous')[] = ['anonymous', 'customer', 'license_admin', 'superadmin'];

/** Path parameters that look real (prefix and shape) but name nothing. */
function paramsFor(route: RouteDef): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of route.path.matchAll(/([a-z-]+)\/:([A-Za-z]+)/g)) {
    const [, collection, name] = m;
    if (name === 'keyId') out[name] = 'key_AAAAAAAAAAAA';
    else if (name === 'email') out[name] = 'nobody@example.org';
    else if (collection === 'licenses') out[name!] = 'lic_AAAAAAAAAAAA';
    else if (collection === 'customers') out[name!] = 'cus_AAAAAAAAAAAA';
    else if (collection === 'requests') out[name!] = 'req_AAAAAAAAAAAA';
    else if (collection === 'business-requests') out[name!] = 'brq_AAAAAAAAAAAA';
    else if (collection === 'order-forms') out[name!] = 'ord_AAAAAAAAAAAA';
    else if (collection === 'users') out[name!] = 'usr_AAAAAAAAAAAA';
    else if (collection === 'blocked-emails') out[name!] = 'nobody@example.org';
    else throw new Error(`no test value for ${route.path} :${name}`);
  }
  return out;
}

function expectation(route: RouteDef, role: Role | 'anonymous'): 'open' | 'unauthenticated' | { missing: Permission } {
  if (route.access === 'anonymous' || route.access === 'public') return 'open';
  if (role === 'anonymous') return 'unauthenticated';
  if (route.access === 'session') return 'open';
  return ROLE_PERMISSIONS[role].includes(route.access) ? 'open' : { missing: route.access };
}

describeDb('RBAC matrix', () => {
  const t = useTestDb();
  let app: TestApp;
  const users = new Map<Role, TestUser>();
  // The IdP is unreachable on purpose: sign-in answers 503, which is not an authorization answer.
  const config: Config = testConfig({ oidc: { issuer: 'http://127.0.0.1:9/dex' } });

  beforeAll(async () => {
    const log = new MemoryLogger();
    app = makeTestApp({ handlers, db: t.db, config, log, pipeline: createPipeline(config, log) });
    for (const role of ['customer', 'license_admin', 'superadmin'] as const) users.set(role, await createUser(t.db, { roles: [role], email: `${role}@example.org` }));
  });
  afterAll(() => app.close());

  test('the matrix covers every route', () => {
    expect(ROUTE_LIST.length).toBeGreaterThanOrEqual(45);
    for (const route of ROUTE_LIST) expect(() => paramsFor(route)).not.toThrow();
  });

  for (const route of ROUTE_LIST) {
    test(`${route.method} ${route.path} (${route.access})`, async () => {
      for (const role of ROLES_UNDER_TEST) {
        // A fresh session per request: logout ends the one it is called with.
        const as = role === 'anonymous' ? null : await signInAs(t.db, users.get(role)!, config);
        const unsafe = route.method !== 'GET';
        const res = await call(app, route.id, {
          as,
          params: paramsFor(route) as never,
          ...(unsafe && route.method !== 'DELETE' ? { body: {} } : {}),
        });
        const text = await res.text();
        const want = expectation(route, role);
        const label = `${role} ${route.method} ${route.path} → ${res.status} ${text.slice(0, 160)}`;
        if (want === 'open') {
          expect(label).not.toMatch(/ → (401|403) /);
        } else if (want === 'unauthenticated') {
          expect(label).toMatch(/ → 401 /);
          expect((JSON.parse(text) as ApiErrorBody).error.key).toBe('unauthenticated');
        } else {
          expect(label).toMatch(/ → 403 /);
          const body = JSON.parse(text) as ApiErrorBody;
          expect(body.error.key).toBe('forbidden');
          expect((body.error.hint as MissingPermissionHint).missing).toBe(want.missing);
        }
      }
    });
  }
});
