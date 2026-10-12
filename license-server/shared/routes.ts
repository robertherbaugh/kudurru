// The route manifest (design spec §8): every HTTP route of the portal with
// its method, path, access rule and flags. It is the single source for the
// server's router (server/http/router.ts builds Bun.serve's routes from it
// and refuses to start while a route has no handler), for the RBAC matrix
// tests, and for the UI, which builds URLs with buildPath. The wire types of
// each route are in shared/api.ts (RouteTypes).
//
// Paths use Bun.serve's parameter syntax (`:id`); Bun hands handlers the
// parameters URL-decoded. Static segments win over parameters
// (/licenses/import before /licenses/:id).

import type { QueryOf } from './api';
import type { Permission } from './permissions';

/**
 * Who may call a route:
 * - anonymous: anyone; no session is read (health, sign-in, the /verify page's POST);
 * - public: anyone, cross-origin from PUBLIC_FORM_ORIGINS; no cookies are read or set, CORS applies;
 * - session: any signed-in user (401 unauthenticated without one);
 * - a Permission: a signed-in user holding it (403 forbidden with hint {missing} otherwise).
 */
export type Access = 'anonymous' | 'public' | 'session' | Permission;

export type RouteMethod = 'GET' | 'POST' | 'PATCH' | 'DELETE';

/**
 * What a route answers: json (default) with a JSON body; download with a
 * license.key attachment (text/plain; charset=us-ascii); redirect with a 302.
 */
export type RouteKind = 'json' | 'download' | 'redirect';

export interface RouteSpec {
  readonly method: RouteMethod;
  readonly path: string;
  readonly access: Access;
  /** Default 'json'. */
  readonly kind?: RouteKind;
  /** The largest request body in bytes; default DEFAULT_BODY_LIMIT. Larger bodies answer 413 payload_too_large. */
  readonly bodyLimit?: number;
  /** CORS for the kudurru.io form (PUBLIC_FORM_ORIGINS; server/http/cors.ts). */
  readonly cors?: 'publicForm';
  /** The route also answers OPTIONS (the CORS preflight, 204). */
  readonly preflight?: true;
  /**
   * 'invalid': a wrong content type, malformed JSON and an oversize body
   * answer 400 invalid instead of 415/400/413 (the frozen public form
   * contract the kudurru.io client is coded to).
   */
  readonly badBody?: 'invalid';
}

/** The default request body limit (the router enforces it; Bun.serve's own cap is higher, server/http/router.ts MAX_REQUEST_BODY). */
export const DEFAULT_BODY_LIMIT = 64 * 1024;

/** The body limit of the public routes. */
export const PUBLIC_BODY_LIMIT = 8 * 1024;

/** Every API path starts with it. */
export const API_PREFIX = '/api/v1';

/** Methods that change state: CSRF applies to them on session and permission routes. */
export const UNSAFE_METHODS: readonly string[] = ['POST', 'PUT', 'PATCH', 'DELETE'];

/** Every route, by RouteId. */
export const ROUTES = {
  // Health (Docker HEALTHCHECK: server/healthcheck.ts).
  healthz: { method: 'GET', path: '/healthz', access: 'anonymous' },
  readyz: { method: 'GET', path: '/readyz', access: 'anonymous' },

  // Sign-in (OIDC, server/auth).
  authConfig: { method: 'GET', path: '/api/v1/auth/config', access: 'anonymous' },
  authLogin: { method: 'GET', path: '/api/v1/auth/login', access: 'anonymous', kind: 'redirect' },
  authCallback: { method: 'GET', path: '/api/v1/auth/callback', access: 'anonymous', kind: 'redirect' },

  // The kudurru.io form (frozen contract) and its confirmation page.
  publicKeyRequest: {
    method: 'POST',
    path: '/api/v1/public/key-requests',
    access: 'public',
    bodyLimit: PUBLIC_BODY_LIMIT,
    cors: 'publicForm',
    preflight: true,
    badBody: 'invalid',
  },
  publicKeyRequestVerify: { method: 'POST', path: '/api/v1/public/key-requests/verify', access: 'anonymous', bodyLimit: PUBLIC_BODY_LIMIT },

  // The session.
  authMe: { method: 'GET', path: '/api/v1/auth/me', access: 'session' },
  authLogout: { method: 'POST', path: '/api/v1/auth/logout', access: 'session' },
  catalog: { method: 'GET', path: '/api/v1/catalog', access: 'session' },

  // The signed-in person's own licenses and requests.
  myLicenses: { method: 'GET', path: '/api/v1/me/licenses', access: 'self.read' },
  myLicense: { method: 'GET', path: '/api/v1/me/licenses/:id', access: 'self.read' },
  myLicenseKey: { method: 'GET', path: '/api/v1/me/licenses/:id/key', access: 'self.read', kind: 'download' },
  myRequests: { method: 'GET', path: '/api/v1/me/requests', access: 'self.read' },
  myRequestCreate: { method: 'POST', path: '/api/v1/me/requests', access: 'self.request' },
  myLicenseRenew: { method: 'POST', path: '/api/v1/me/licenses/:id/renew', access: 'self.request' },
  // The /invites/:token page names the customer before the person accepts
  // (spec §7). POST keeps the token out of URLs and logs; nothing changes.
  myInvitePreview: { method: 'POST', path: '/api/v1/me/invites/preview', access: 'self.request' },
  myInviteRedeem: { method: 'POST', path: '/api/v1/me/invites/redeem', access: 'self.request' },

  // Customers and their members.
  customersList: { method: 'GET', path: '/api/v1/customers', access: 'customers.read' },
  customerCreate: { method: 'POST', path: '/api/v1/customers', access: 'customers.write' },
  customerGet: { method: 'GET', path: '/api/v1/customers/:id', access: 'customers.read' },
  customerUpdate: { method: 'PATCH', path: '/api/v1/customers/:id', access: 'customers.write' },
  memberAdd: { method: 'POST', path: '/api/v1/customers/:id/members', access: 'customers.write' },
  memberInvite: { method: 'POST', path: '/api/v1/customers/:id/members/:email/invite', access: 'customers.write' },
  memberRemove: { method: 'DELETE', path: '/api/v1/customers/:id/members/:email', access: 'customers.write' },

  // Licenses.
  licensesList: { method: 'GET', path: '/api/v1/licenses', access: 'licenses.read' },
  licenseIssue: { method: 'POST', path: '/api/v1/licenses', access: 'licenses.issue' },
  licenseImport: { method: 'POST', path: '/api/v1/licenses/import', access: 'licenses.issue' },
  licenseGet: { method: 'GET', path: '/api/v1/licenses/:id', access: 'licenses.read' },
  licenseKeyDownload: { method: 'GET', path: '/api/v1/licenses/:id/keys/:keyId/download', access: 'licenses.read', kind: 'download' },
  licenseRenew: { method: 'POST', path: '/api/v1/licenses/:id/renew', access: 'licenses.renew' },
  licenseChange: { method: 'POST', path: '/api/v1/licenses/:id/change', access: 'licenses.change' },
  licenseWithdraw: { method: 'POST', path: '/api/v1/licenses/:id/withdraw', access: 'licenses.withdraw' },
  licenseEmail: { method: 'POST', path: '/api/v1/licenses/:id/email', access: 'licenses.issue' },

  // Personal key requests that wait for review, and the blocklist.
  requestsList: { method: 'GET', path: '/api/v1/requests', access: 'requests.read' },
  requestApprove: { method: 'POST', path: '/api/v1/requests/:id/approve', access: 'requests.decide' },
  requestReject: { method: 'POST', path: '/api/v1/requests/:id/reject', access: 'requests.decide' },
  blockedEmailsList: { method: 'GET', path: '/api/v1/blocked-emails', access: 'blocklist.read' },
  blockedEmailRemove: { method: 'DELETE', path: '/api/v1/blocked-emails/:email', access: 'blocklist.write' },

  // Users, the audit log and the System page.
  usersList: { method: 'GET', path: '/api/v1/users', access: 'users.read' },
  userGet: { method: 'GET', path: '/api/v1/users/:id', access: 'users.read' },
  userUpdate: { method: 'PATCH', path: '/api/v1/users/:id', access: 'users.write' },
  userRevokeSessions: { method: 'POST', path: '/api/v1/users/:id/revoke-sessions', access: 'users.write' },
  auditList: { method: 'GET', path: '/api/v1/audit', access: 'audit.read' },
  system: { method: 'GET', path: '/api/v1/system', access: 'system.read' },
} as const satisfies Record<string, RouteSpec>;

/** The name of a route. */
export type RouteId = keyof typeof ROUTES;

/** A route with its name. */
export type RouteDef<Id extends RouteId = RouteId> = (typeof ROUTES)[Id] & RouteSpec & { readonly id: Id };

/** Every route with its name, in manifest order. */
export const ROUTE_LIST: readonly RouteDef[] = (Object.keys(ROUTES) as RouteId[]).map((id) => routeDef(id));

/** The route by name, with its id. */
export function routeDef<Id extends RouteId>(id: Id): RouteDef<Id> {
  const def: RouteDef<Id> = { ...ROUTES[id], id };
  return def;
}

/** The `:name` parameters of a path, as a union of names. */
type ParamNames<P extends string> = P extends `${string}:${infer Name}/${infer Rest}`
  ? Name | ParamNames<Rest>
  : P extends `${string}:${infer Name}`
    ? Name
    : never;

/** The path parameters of a route ({} when it has none). */
export type RouteParams<Id extends RouteId> = { readonly [K in ParamNames<(typeof ROUTES)[Id]['path']>]: string };

/** Whether a route has path parameters. */
type HasParams<Id extends RouteId> = [ParamNames<(typeof ROUTES)[Id]['path']>] extends [never] ? false : true;

/** The query of buildPath: the route's query type, or plain pairs. */
export type QueryInit<Id extends RouteId> = QueryOf<Id> | URLSearchParams | Readonly<Record<string, string | number | boolean | null | undefined>>;

/**
 * The URL path of a route, with its parameters encoded
 * (encodeURIComponent) and an optional query string; null and undefined
 * query values are left out.
 *
 *   buildPath('licenseGet', { id: 'lic_x' })                    → /api/v1/licenses/lic_x
 *   buildPath('memberRemove', { id: 'cus_x', email: 'a@b.c' })  → /api/v1/customers/cus_x/members/a%40b.c
 *   buildPath('licensesList', {}, { plan: 'personal' })          → /api/v1/licenses?plan=personal
 */
export function buildPath<Id extends RouteId>(
  id: Id,
  ...args: HasParams<Id> extends true ? [params: RouteParams<Id>, query?: QueryInit<Id>] : [params?: RouteParams<Id>, query?: QueryInit<Id>]
): string {
  const [params, query] = args;
  const values = (params ?? {}) as Readonly<Record<string, string>>;
  const path = ROUTES[id].path.replace(/:([A-Za-z]+)/g, (_, name: string) => {
    const v = values[name];
    if (v === undefined || v === '') throw new Error(`buildPath(${id}): missing parameter ${name}`);
    return encodeURIComponent(v);
  });
  if (!query) return path;
  const qs = query instanceof URLSearchParams ? new URLSearchParams(query) : new URLSearchParams();
  if (!(query instanceof URLSearchParams)) {
    for (const [k, v] of Object.entries(query as Readonly<Record<string, string | number | boolean | null | undefined>>)) {
      if (v === undefined || v === null) continue;
      qs.set(k, String(v));
    }
  }
  const s = qs.toString();
  return s ? `${path}?${s}` : path;
}
