// MSW handlers for the IAM reads and the reviewed IAM writers (internal/api
// iamwriters.go, cmd/kudurru serving_http_iam_writers.go, D294, D396): local
// accounts, ServiceAccounts with their tokens, IdentityProviders and
// RoleBindings. Like the server they decode strictly, compare the exact UID
// and resourceVersion, refuse the built-in admin and other managers, answer
// 503 user_management_unavailable / iam_management_unavailable when the
// driver does not offer the commands, and never echo a password or a token
// after the 201. Tests change the state with mockIAM() (a failure for the
// next command, the request log).
import { http, HttpResponse, type JsonBodyType } from 'msw';
import type { ApiToken, IamIdentityProvider, IdentityProviderDesired, IdentityProviderDocument, RoleBindingDesired, RoleBindingDocument, ServiceAccountDesired, ServiceAccountDocument } from '../api/types';
import type { MockDB, MockUser } from './fixtures';

type Guard = (r: Request, resource: string, verb: string) => Response | null;

const B = '*/api/v1';
const API = 'kudurru.io/v1alpha1';
const MANAGED = 'kudurru.io/managed-by';

export interface IAMMock {
  serviceAccounts: ServiceAccountDocument[];
  identityProviders: IdentityProviderDocument[];
  /** Providers of the configuration file (no object, no writer). */
  configProviders: IamIdentityProvider[];
  /** RoleBinding objects (the config binding is added by the list). */
  roleBindings: RoleBindingDocument[];
  /** The next IAM command answers this instead of running (status, key). */
  failNext: { status: number; key: string; message: string; hint?: string } | null;
  /** Mutating requests, newest last: method, path and body (tests assert on them). */
  calls: { method: string; path: string; body: unknown }[];
  /** Token secrets issued, so a test can check one is never shown again. */
  issued: string[];
  seq: number;
}

function fixture(): IAMMock {
  const sa = (namespace: string, name: string, description: string, manager: string, rv: string): ServiceAccountDocument => ({
    apiVersion: API, kind: 'ServiceAccount',
    metadata: { namespace, name, uid: `sa-${namespace}-${name}`, resourceVersion: rv, annotations: { [MANAGED]: manager } },
    spec: { ...(description ? { description } : {}) },
  });
  const rb = (namespace: string, name: string, manager: string, role: string, scope: RoleBindingDocument['spec']['scope'], subjects: RoleBindingDocument['spec']['subjects']): RoleBindingDocument => ({
    apiVersion: API, kind: 'RoleBinding',
    metadata: { namespace, name, uid: `rb-${namespace}-${name}`, resourceVersion: '7', ...(manager ? { annotations: { [MANAGED]: manager } } : {}) },
    spec: { role, scope, subjects },
  });
  return {
    serviceAccounts: [sa('default', 'grafana', 'dashboards', 'api', '11'), sa('kudurru', 'external-dns', '', 'file:objects.d/external-dns.yaml', '12')],
    identityProviders: [
      {
        apiVersion: API, kind: 'IdentityProvider',
        metadata: { name: 'github', uid: 'idp-github', resourceVersion: '21', annotations: { [MANAGED]: 'api' }, labels: { team: 'platform' } },
        spec: {
          displayName: 'GitHub', issuer: 'https://token.actions.githubusercontent.com', clientID: 'kudurru-gh', scopes: ['openid'],
          redirectURL: 'https://kudurru.example.com/api/v1/auth/oidc/github/callback', clientSecretRef: { env: 'GITHUB_CLIENT_SECRET' },
          claimMappings: { username: { claim: 'sub', prefix: 'gh:' } }, provisioning: { autoProvision: 'never' },
        },
      },
    ],
    configProviders: [
      { id: 'dex', displayName: 'Dex (corporate SSO)', issuer: 'https://dex.example.com', clientID: 'kudurru', scopes: ['openid', 'email', 'groups'], redirectURL: 'https://kudurru.example.com/api/v1/auth/oidc/dex/callback', source: 'config' },
    ],
    roleBindings: [
      rb('default', 'viewers', 'file:objects.d/rbac.yaml', 'viewer', { kind: 'cluster' }, [{ kind: 'Group', name: 'everyone' }]),
      rb('default', 'home-admin', 'api', 'zone-admin', { kind: 'zone', name: 'home-arpa' }, [{ kind: 'User', name: 'robert@example.com' }]),
      rb('kudurru', 'external-dns', 'file:objects.d/external-dns.yaml', 'integration:external-dns', { kind: 'cluster' }, [{ kind: 'ServiceAccount', namespace: 'kudurru', name: 'external-dns' }]),
    ],
    failNext: null,
    calls: [],
    issued: [],
    seq: 100,
  };
}

let state = fixture();
export const resetIAM = () => {
  state = fixture();
};
/** The live IAM state, for tests. */
export const mockIAM = () => state;

function fail(status: number, key: string, message: string, hint?: string) {
  return HttpResponse.json({ error: { key, message, ...(hint ? { hint } : {}) }, took_ms: 0.4 } as JsonBodyType, { status });
}
const ok = (body: unknown, status = 200) => HttpResponse.json(body as JsonBodyType, { status });
const noContent = () => new HttpResponse(null, { status: 204 });

/** The uid of a mock account (fixture accounts keep their historic uid). */
export function userUID(u: MockUser): string {
  return u.uid ?? `mock-user-${u.username}`;
}
const userRV = (u: MockUser) => u.resourceVersion ?? '1';

function iamUser(u: MockUser, now: number) {
  return {
    name: u.username,
    uid: userUID(u),
    resourceVersion: userRV(u),
    ...(u.name !== u.username ? { displayName: u.name } : {}),
    builtinAdmin: u.username === 'admin',
    disabled: !!u.disabled,
    mustChangePassword: !!u.mustChangePassword,
    groups: u.groups ?? (u.roles.includes('admin') ? ['kudurru-admins'] : []),
    managedBy: u.username === 'admin' ? 'bootstrap:admin' : (u.managedBy ?? 'api'),
    ...(u.lastLoginAt !== null ? { lastLoginAt: u.lastLoginAt ?? new Date(now - 3_600_000).toISOString() } : {}),
  };
}

/** Strict JSON object decode: unknown keys refused like the server's decoder. */
async function decode(request: Request, allowed: string[]): Promise<Record<string, unknown> | Response> {
  let body: unknown;
  try {
    body = await request.json();
  } catch {
    return fail(400, 'invalid', 'invalid request body');
  }
  if (!body || typeof body !== 'object' || Array.isArray(body)) return fail(400, 'invalid', 'invalid request body');
  const extra = Object.keys(body).find((k) => !allowed.includes(k));
  if (extra) return fail(400, 'invalid', 'invalid request body');
  state.calls.push({ method: request.method, path: new URL(request.url).pathname, body: redact(body as Record<string, unknown>) });
  return body as Record<string, unknown>;
}

/** The request log never keeps a password. */
function redact(body: Record<string, unknown>): Record<string, unknown> {
  const out = { ...body };
  for (const k of ['password', 'newPassword']) if (k in out) out[k] = typeof out[k] === 'string' ? `<${(out[k]).length} chars>` : out[k];
  return out;
}

const review = (b: Record<string, unknown>) => ({ uid: typeof b.uid === 'string' ? b.uid : '', resourceVersion: typeof b.resourceVersion === 'string' ? b.resourceVersion : '' });

export function iamHandlers(guard: Guard, getDB: () => MockDB) {
  const injected = () => {
    const f = state.failNext;
    if (!f) return null;
    state.failNext = null;
    return fail(f.status, f.key, f.message, f.hint);
  };
  const users = () => (getDB().authCapabilities.localUserCommands ? null : fail(503, 'user_management_unavailable', 'local user management is unavailable on this driver', 'manage local accounts with kudurru admin or your identity provider'));
  const objects = () => (getDB().authCapabilities.iamObjectCommands !== false ? null : fail(503, 'iam_management_unavailable', 'IAM object commands are unavailable on this driver', 'change ServiceAccounts and IdentityProviders with kubectl'));
  const next = () => String(++state.seq);
  const findUser = (name: string) => getDB().users.find((u) => u.username === name);
  const reviewedUser = (name: string, r: { uid: string; resourceVersion: string }): MockUser | Response => {
    const u = findUser(name);
    if (!u) return fail(404, 'not_found', 'resource not found');
    if (userUID(u) !== r.uid || userRV(u) !== r.resourceVersion) return fail(409, 'conflict', 'local account changed or already exists; refresh before making a new request');
    if (u.username === 'admin') return fail(403, 'forbidden', 'permission denied');
    if ((u.managedBy ?? 'api') !== 'api') return fail(409, 'managed_by', 'local account or credential belongs to another manager');
    return u;
  };
  const endSessions = (u: MockUser) => {
    const db = getDB();
    db.tokens = db.tokens.filter((t) => !(t.owner.kind === 'User' && t.owner.name === u.username));
    if (db.session?.user === u) db.session = null;
  };
  const saKey = (ns: string, name: string) => state.serviceAccounts.findIndex((s) => s.metadata.namespace === ns && s.metadata.name === name);
  const reviewedObject = <T extends { metadata: { uid?: string; resourceVersion?: string; annotations?: Record<string, string> } }>(o: T | undefined, r: { uid: string; resourceVersion: string }, kind: string): T | Response => {
    if (!o) return fail(404, 'not_found', 'resource not found');
    if (o.metadata.uid !== r.uid || o.metadata.resourceVersion !== r.resourceVersion) return fail(409, 'conflict', `${kind} changed or already exists; review before making a new request`);
    if (o.metadata.annotations?.[MANAGED] !== 'api') return fail(409, 'managed_by', `${kind} belongs to another manager`);
    return o;
  };
  const managerless = (meta: { annotations?: Record<string, string> } | undefined) => !meta?.annotations || !(MANAGED in meta.annotations);
  const saTokens = (ns: string, name: string) => getDB().tokens.filter((t) => t.owner.kind === 'ServiceAccount' && t.owner.namespace === ns && t.owner.name === name).length;

  return [
    // --- reads
    http.get(`${B}/iam/users`, ({ request }) => guard(request, 'users', 'read') ?? ok({ items: getDB().users.map((u) => iamUser(u, getDB().now)), took_ms: 1 })),
    http.get(`${B}/iam/serviceaccounts`, ({ request }) =>
      guard(request, 'serviceaccounts', 'read') ??
      ok({
        items: state.serviceAccounts.map((s) => ({
          namespace: s.metadata.namespace, name: s.metadata.name, ...(s.spec.description ? { description: s.spec.description } : {}),
          disabled: !!s.spec.disabled, builtinAdmin: false, tokens: saTokens(s.metadata.namespace ?? '', s.metadata.name ?? ''),
          ...(s.metadata.annotations?.[MANAGED] ? { managedBy: s.metadata.annotations[MANAGED] } : {}),
        })),
        took_ms: 1,
      }),
    ),
    http.get(`${B}/iam/serviceaccounts/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'serviceaccounts', 'read');
      if (g) return g;
      const s = state.serviceAccounts[saKey(String(params.ns), String(params.name))];
      return s ? ok(s) : fail(404, 'not_found', 'resource not found');
    }),
    http.get(`${B}/iam/identityproviders`, ({ request }) =>
      guard(request, 'identityproviders', 'read') ??
      ok({
        items: [
          ...state.configProviders,
          ...state.identityProviders.map((p) => ({
            id: p.metadata.name, displayName: p.spec.displayName ?? '', issuer: p.spec.issuer, clientID: p.spec.clientID, scopes: p.spec.scopes ?? null,
            redirectURL: p.spec.redirectURL ?? '', ...(p.spec.cliClientID ? { cliClientID: p.spec.cliClientID } : {}), source: 'object',
          })),
        ],
        took_ms: 1,
      }),
    ),
    http.get(`${B}/iam/identityproviders/:name`, ({ request, params }) => {
      const g = guard(request, 'identityproviders', 'read');
      if (g) return g;
      const p = state.identityProviders.find((x) => x.metadata.name === params.name);
      return p ? ok(p) : fail(404, 'not_found', 'resource not found');
    }),
    http.get(`${B}/iam/rolebindings`, ({ request }) =>
      guard(request, 'rolebindings', 'read') ??
      ok({
        items: [
          { source: 'config', name: 'rbac.bindings[0]', role: 'admin', scope: { kind: 'cluster' }, subjects: [{ kind: 'Group', name: 'kudurru-admins' }] },
          ...state.roleBindings.map((b) => ({ source: 'object', namespace: b.metadata.namespace, name: b.metadata.name, role: b.spec.role, scope: b.spec.scope ?? { kind: 'cluster' }, subjects: b.spec.subjects, ...(b.metadata.annotations?.[MANAGED] ? { managedBy: b.metadata.annotations[MANAGED] } : {}) })),
        ],
        took_ms: 1,
      }),
    ),
    http.get(`${B}/iam/rolebindings/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'rolebindings', 'read');
      if (g) return g;
      const b = state.roleBindings.find((x) => x.metadata.namespace === params.ns && x.metadata.name === params.name);
      return b ? ok(b) : fail(404, 'not_found', 'resource not found');
    }),

    // --- local accounts
    http.post(`${B}/iam/users`, async ({ request }) => {
      const g = guard(request, 'users', 'write') ?? users();
      if (g) return g;
      const body = await decode(request, ['name', 'displayName', 'password']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const name = typeof body.name === 'string' ? body.name : '';
      if (!/^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/.test(name) || typeof body.password !== 'string' || [...body.password].length < 8) return fail(400, 'invalid', 'invalid local account request');
      if (findUser(name)) return fail(409, 'conflict', 'local account changed or already exists; refresh before making a new request');
      const u: MockUser = { username: name, password: body.password, name: typeof body.displayName === 'string' && body.displayName ? body.displayName : name, roles: [], mustChangePassword: true, uid: `mock-user-${name}-${next()}`, resourceVersion: next(), groups: [], managedBy: 'api', lastLoginAt: null };
      getDB().users.push(u);
      return ok(iamUser(u, getDB().now), 201);
    }),
    http.patch(`${B}/iam/users/:name`, async ({ request, params }) => {
      const g = guard(request, 'users', 'write') ?? users();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion', 'displayName', 'groups', 'disabled']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      if (!('displayName' in body) && !('groups' in body) && !('disabled' in body)) return fail(400, 'invalid', 'exact uid and resourceVersion and at least one changed field are required');
      if (Array.isArray(body.groups) && body.groups.some((x, i, all) => typeof x !== 'string' || !x || x.includes(':') || x.trim() !== x || all.indexOf(x) !== i)) return fail(400, 'invalid', 'invalid or duplicate local group name');
      const u = reviewedUser(String(params.name), review(body));
      if (u instanceof Response) return u;
      let revoked = false;
      if (typeof body.displayName === 'string') u.name = body.displayName || u.username;
      if (Array.isArray(body.groups)) {
        const groups = body.groups as string[];
        revoked ||= JSON.stringify(groups) !== JSON.stringify(u.groups ?? []);
        u.groups = groups;
      }
      if (typeof body.disabled === 'boolean') {
        revoked ||= body.disabled !== !!u.disabled;
        u.disabled = body.disabled;
      }
      u.resourceVersion = next();
      if (revoked) endSessions(u);
      return ok(iamUser(u, getDB().now));
    }),
    http.delete(`${B}/iam/users/:name`, async ({ request, params }) => {
      const g = guard(request, 'users', 'delete') ?? users();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const u = reviewedUser(String(params.name), review(body));
      if (u instanceof Response) return u;
      endSessions(u);
      getDB().users = getDB().users.filter((x) => x !== u);
      return noContent();
    }),
    http.post(`${B}/iam/users/:name/password`, async ({ request, params }) => {
      const g = guard(request, 'users', 'write') ?? users();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion', 'newPassword']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      if (typeof body.newPassword !== 'string' || [...body.newPassword].length < 8) return fail(400, 'invalid', 'invalid local account request');
      const u = reviewedUser(String(params.name), review(body));
      if (u instanceof Response) return u;
      u.password = body.newPassword;
      u.mustChangePassword = true;
      u.resourceVersion = next();
      endSessions(u);
      return noContent();
    }),

    // --- service accounts
    http.post(`${B}/iam/serviceaccounts`, async ({ request }) => {
      const g = guard(request, 'serviceaccounts', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['apiVersion', 'kind', 'metadata', 'spec']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const d = body as unknown as ServiceAccountDesired;
      if (d.apiVersion !== API || d.kind !== 'ServiceAccount' || !managerless(d.metadata) || Object.keys(d.spec ?? {}).some((k) => !['description', 'disabled'].includes(k))) return fail(400, 'invalid', 'invalid ServiceAccount command');
      if (saKey(d.metadata.namespace, d.metadata.name) >= 0) return fail(409, 'conflict', 'ServiceAccount changed or already exists; review before making a new request');
      const doc: ServiceAccountDocument = { apiVersion: API, kind: 'ServiceAccount', metadata: { ...d.metadata, uid: `sa-${d.metadata.namespace}-${d.metadata.name}-${next()}`, resourceVersion: next(), annotations: { ...(d.metadata.annotations ?? {}), [MANAGED]: 'api' } }, spec: { ...d.spec } };
      state.serviceAccounts.push(doc);
      return ok(doc, 201);
    }),
    http.put(`${B}/iam/serviceaccounts/:ns/:name`, async ({ request, params }) => {
      const g = guard(request, 'serviceaccounts', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion', 'serviceAccount']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const d = body.serviceAccount as ServiceAccountDesired;
      if (!d || d.apiVersion !== API || d.kind !== 'ServiceAccount' || !managerless(d.metadata) || d.metadata.namespace !== params.ns || d.metadata.name !== params.name) return fail(400, 'invalid', 'invalid ServiceAccount command');
      const i = saKey(String(params.ns), String(params.name));
      const old = reviewedObject(state.serviceAccounts[i], review(body), 'ServiceAccount');
      if (old instanceof Response) return old;
      const disabling = !!d.spec.disabled && !old.spec.disabled;
      const doc: ServiceAccountDocument = { ...old, metadata: { ...old.metadata, labels: d.metadata.labels, annotations: { ...(d.metadata.annotations ?? {}), [MANAGED]: 'api' }, resourceVersion: next() }, spec: { ...d.spec } };
      state.serviceAccounts[i] = doc;
      if (disabling) getDB().tokens = getDB().tokens.filter((t) => !(t.owner.kind === 'ServiceAccount' && t.owner.namespace === params.ns && t.owner.name === params.name));
      return ok(doc);
    }),
    http.delete(`${B}/iam/serviceaccounts/:ns/:name`, async ({ request, params }) => {
      const g = guard(request, 'serviceaccounts', 'delete') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const i = saKey(String(params.ns), String(params.name));
      const old = reviewedObject(state.serviceAccounts[i], review(body), 'ServiceAccount');
      if (old instanceof Response) return old;
      state.serviceAccounts.splice(i, 1);
      getDB().tokens = getDB().tokens.filter((t) => !(t.owner.kind === 'ServiceAccount' && t.owner.namespace === params.ns && t.owner.name === params.name));
      return noContent();
    }),
    http.post(`${B}/iam/serviceaccounts/:ns/:name/tokens`, async ({ request, params }) => {
      const g = guard(request, 'serviceaccounts', 'write') ?? guard(request, 'tokens', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'description', 'expiresIn', 'scopes']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const s = state.serviceAccounts[saKey(String(params.ns), String(params.name))];
      if (!s) return fail(404, 'not_found', 'resource not found');
      if (s.metadata.uid !== body.uid) return fail(409, 'conflict', 'ServiceAccount changed or already exists; review before making a new request');
      if (s.metadata.annotations?.[MANAGED] !== 'api') return fail(409, 'managed_by', 'ServiceAccount belongs to another manager');
      if (s.spec.disabled) return fail(409, 'disabled', 'the ServiceAccount is disabled; enable it before issuing a token');
      const expiresIn = typeof body.expiresIn === 'string' ? body.expiresIn : '';
      if (expiresIn && !/^\d+(h|m|s)$/.test(expiresIn)) return fail(400, 'invalid', 'expiresIn must be a positive Go duration');
      const id = `sa${next()}`;
      const secret = `kdr_${id}_${Math.random().toString(36).slice(2, 14)}`;
      const t: ApiToken = {
        id,
        ...(typeof body.description === 'string' && body.description ? { description: body.description } : {}),
        owner: { kind: 'ServiceAccount', name: String(params.name), namespace: String(params.ns), id: `sa:${String(params.ns)}/${String(params.name)}`, provider: 'kudurru', uid: s.metadata.uid ?? '' },
        createdAt: new Date().toISOString(),
        expiresAt: expiresIn ? new Date(Date.now() + 3_600_000).toISOString() : null,
        ...(Array.isArray(body.scopes) && body.scopes.length ? { scopes: body.scopes as string[] } : {}),
      };
      getDB().tokens.push(t);
      state.issued.push(secret);
      return ok({ ...t, token: secret }, 201);
    }),

    // --- identity providers
    http.post(`${B}/iam/identityproviders`, async ({ request }) => {
      const g = guard(request, 'identityproviders', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['apiVersion', 'kind', 'metadata', 'spec']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const d = body as unknown as IdentityProviderDesired;
      const refs = d.spec?.clientSecretRef ? Object.keys(d.spec.clientSecretRef).length : 0;
      if (d.apiVersion !== API || d.kind !== 'IdentityProvider' || !managerless(d.metadata) || !d.spec?.issuer || !d.spec.clientID || refs !== 1) return fail(400, 'invalid', 'invalid IdentityProvider fields');
      if (state.configProviders.some((p) => p.id === d.metadata.name)) return fail(409, 'conflict', 'the node configuration defines a provider with this id');
      if (state.identityProviders.some((p) => p.metadata.name === d.metadata.name)) return fail(409, 'conflict', 'IdentityProvider changed or already exists; review before making a new request');
      const doc: IdentityProviderDocument = { apiVersion: API, kind: 'IdentityProvider', metadata: { ...d.metadata, uid: `idp-${d.metadata.name}-${next()}`, resourceVersion: next(), annotations: { ...(d.metadata.annotations ?? {}), [MANAGED]: 'api' } }, spec: structuredClone(d.spec) };
      state.identityProviders.push(doc);
      return ok(doc, 201);
    }),
    http.put(`${B}/iam/identityproviders/:name`, async ({ request, params }) => {
      const g = guard(request, 'identityproviders', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion', 'identityProvider']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const d = body.identityProvider as IdentityProviderDesired;
      const refs = d?.spec?.clientSecretRef ? Object.keys(d.spec.clientSecretRef).length : 0;
      if (!d || d.apiVersion !== API || d.kind !== 'IdentityProvider' || !managerless(d.metadata) || d.metadata.name !== params.name || refs !== 1) return fail(400, 'invalid', 'invalid IdentityProvider fields');
      const i = state.identityProviders.findIndex((p) => p.metadata.name === params.name);
      const old = reviewedObject(state.identityProviders[i], review(body), 'IdentityProvider');
      if (old instanceof Response) return old;
      const doc: IdentityProviderDocument = { ...old, metadata: { ...old.metadata, labels: d.metadata.labels, annotations: { ...(d.metadata.annotations ?? {}), [MANAGED]: 'api' }, resourceVersion: next() }, spec: structuredClone(d.spec) };
      state.identityProviders[i] = doc;
      return ok(doc);
    }),
    http.delete(`${B}/iam/identityproviders/:name`, async ({ request, params }) => {
      const g = guard(request, 'identityproviders', 'delete') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const i = state.identityProviders.findIndex((p) => p.metadata.name === params.name);
      const old = reviewedObject(state.identityProviders[i], review(body), 'IdentityProvider');
      if (old instanceof Response) return old;
      state.identityProviders.splice(i, 1);
      return noContent();
    }),

    // --- role bindings (create stays in handlers.ts: it records createdRoleBindings)
    http.put(`${B}/iam/rolebindings/:ns/:name`, async ({ request, params }) => {
      const g = guard(request, 'rolebindings', 'write') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion', 'binding']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const d = body.binding as RoleBindingDesired;
      if (!d || d.kind !== 'RoleBinding' || d.metadata.namespace !== params.ns || d.metadata.name !== params.name) return fail(400, 'invalid', 'invalid RoleBinding command');
      const i = state.roleBindings.findIndex((b) => b.metadata.namespace === params.ns && b.metadata.name === params.name);
      const old = reviewedObject(state.roleBindings[i], review(body), 'RoleBinding');
      if (old instanceof Response) return old;
      const doc: RoleBindingDocument = { ...old, metadata: { ...old.metadata, resourceVersion: next() }, spec: structuredClone(d.spec) };
      state.roleBindings[i] = doc;
      return ok(doc);
    }),
    http.delete(`${B}/iam/rolebindings/:ns/:name`, async ({ request, params }) => {
      const g = guard(request, 'rolebindings', 'delete') ?? objects();
      if (g) return g;
      const body = await decode(request, ['uid', 'resourceVersion']);
      if (body instanceof Response) return body;
      const f = injected();
      if (f) return f;
      const i = state.roleBindings.findIndex((b) => b.metadata.namespace === params.ns && b.metadata.name === params.name);
      const old = reviewedObject(state.roleBindings[i], review(body), 'RoleBinding');
      if (old instanceof Response) return old;
      state.roleBindings.splice(i, 1);
      return noContent();
    }),
  ];
}

/** Records a RoleBinding created by POST /iam/rolebindings as a stored object (handlers.ts). */
export function storeCreatedRoleBinding(body: RoleBindingDesired): RoleBindingDocument {
  const doc: RoleBindingDocument = { apiVersion: API, kind: 'RoleBinding', metadata: { name: body.metadata.name, namespace: body.metadata.namespace, uid: `rb-${body.metadata.name}`, resourceVersion: String(++state.seq), annotations: { [MANAGED]: 'api' } }, spec: structuredClone(body.spec) };
  state.roleBindings.push(doc);
  return doc;
}
